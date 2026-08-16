// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
)

// The in-process broker (🎯T2.1): bind via Listen, accept connections, and
// speak the wire contract. Spawn here allocates a session record rather than
// starting a provider process — the pool and the real spawn live in 🎯T2.3 /
// 🎯T3. The PID on the wire is a handle so the field is populated, not a live
// process the caller may signal.

// tailBuffer is how many events a subscriber may queue before the broker
// treats it as lagged. The contract forbids silently dropping events: a
// lagged tail is closed rather than truncated.
const tailBuffer = 64

// ServeArgs configures Serve.
type ServeArgs struct {
	// Listener is the bound socket. Required.
	Listener net.Listener
	// Clock stamps tail events. Nil means SystemClock.
	Clock Clock
}

// Server accepts broker connections on a Unix socket.
type Server struct {
	ln    net.Listener
	clock Clock

	mu       sync.Mutex
	seq      int
	pids     int
	requests int
	sessions map[string]*managedSession
	tails    []*tailSub
	conns    map[net.Conn]struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

type tailSub struct {
	ch     chan EventMessage
	conn   *Conn
	closed bool
}

type managedSession struct {
	status SessionStatus
	owner  *connState
}

type connState struct {
	held map[string]struct{}
}

// ListenAndServe binds SocketPath and serves until Close.
func ListenAndServe() (*Server, error) {
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	ln, err := Listen(path)
	if err != nil {
		return nil, err
	}
	return Serve(&ServeArgs{Listener: ln})
}

// Serve accepts connections on args.Listener until Close.
func Serve(args *ServeArgs) (*Server, error) {
	if args == nil || args.Listener == nil {
		return nil, fmt.Errorf("broker: Serve requires a Listener")
	}
	clock := args.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	s := &Server{
		ln:       args.Listener,
		clock:    clock,
		sessions: map[string]*managedSession{},
		conns:    map[net.Conn]struct{}{},
		done:     make(chan struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Close stops accepting and closes every connection.
func (s *Server) Close() error {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil
	default:
		close(s.done)
	}
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	tails := append([]*tailSub(nil), s.tails...)
	s.mu.Unlock()

	for _, t := range tails {
		s.dropTail(t)
	}
	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// RequestCount is how many wire requests this server has handled. Tests use
// it to observe whether the library consult reached the socket.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			_ = nc.Close()
			return
		default:
		}
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(nc)
		}()
	}
}

func (s *Server) handleConn(nc net.Conn) {
	c := NewConn(nc)
	owner := &connState{held: map[string]struct{}{}}
	defer func() {
		s.reclaim(owner)
		s.dropConn(nc)
		_ = nc.Close()
	}()

	tailing := false
	for {
		req, err := c.ReadRequest()
		if err != nil {
			if err == io.EOF {
				return
			}
			s.noteRequest()
			if !replyErr(c, "", err) {
				return
			}
			continue
		}
		s.noteRequest()
		if tailing {
			// The connection is a tail stream. Further requests are not
			// honoured; we keep reading only to notice the client hang-up.
			continue
		}
		switch req.Type {
		case TypeSpawn:
			if !s.handleSpawn(c, owner, req) {
				return
			}
		case TypeRelease:
			if !s.handleRelease(c, owner, req) {
				return
			}
		case TypeStatus:
			if !s.handleStatus(c, req) {
				return
			}
		case TypeTail:
			if !s.handleTail(c, req) {
				return
			}
			tailing = true
		}
	}
}

func (s *Server) handleSpawn(c *Conn, owner *connState, req *Request) bool {
	s.mu.Lock()
	s.seq++
	s.pids++
	id := fmt.Sprintf("s-%d", s.seq)
	pid := s.pids
	st := SessionStatus{
		SessionID: id,
		Provider:  req.Spawn.Provider,
		Mode:      req.Spawn.Mode,
		Model:     req.Spawn.Model,
		Intent:    req.Spawn.Intent,
		WorkDir:   req.Spawn.WorkDir,
		PID:       pid,
	}
	s.sessions[id] = &managedSession{status: st, owner: owner}
	owner.held[id] = struct{}{}
	ev := EventMessage{Kind: EventSpawn, SessionID: id, At: s.clock.Now()}
	s.mu.Unlock()

	if err := c.WriteResponse(&Response{
		ID:   req.ID,
		Type: TypeSpawned,
		Spawned: &SpawnResponse{
			SessionID: id,
			PID:       pid,
			Warm:      false,
		},
	}); err != nil {
		return false
	}
	s.emit(ev)
	return true
}

func (s *Server) handleRelease(c *Conn, owner *connState, req *Request) bool {
	rel := req.Release
	if rel.Disposition == DispositionReuse {
		return replyErr(c, req.ID, &ProtocolError{
			Code:  CodeUnsupportedValue,
			Field: "disposition",
			Value: string(DispositionReuse),
			Msg:   "disposition reuse needs the shared pool (🎯T2.3); this broker only honours stop",
		})
	}

	s.mu.Lock()
	sess, ok := s.sessions[rel.SessionID]
	if !ok {
		s.mu.Unlock()
		return replyErr(c, req.ID, &ProtocolError{
			Code:  CodeUnknownSession,
			Field: "session_id",
			Value: rel.SessionID,
			Msg:   fmt.Sprintf("session %s is not one the broker holds", rel.SessionID),
		})
	}
	if sess.owner != owner {
		s.mu.Unlock()
		return replyErr(c, req.ID, &ProtocolError{
			Code:  CodeNotOwner,
			Field: "session_id",
			Value: rel.SessionID,
			Msg:   fmt.Sprintf("session %s belongs to another connection", rel.SessionID),
		})
	}
	delete(s.sessions, rel.SessionID)
	delete(owner.held, rel.SessionID)
	ev := EventMessage{Kind: EventRelease, SessionID: rel.SessionID, At: s.clock.Now()}
	s.mu.Unlock()

	if err := c.WriteResponse(&Response{
		ID:   req.ID,
		Type: TypeReleased,
		Released: &ReleaseResponse{
			SessionID:   rel.SessionID,
			Disposition: DispositionStop,
		},
	}); err != nil {
		return false
	}
	s.emit(ev)
	return true
}

func (s *Server) handleStatus(c *Conn, req *Request) bool {
	s.mu.Lock()
	sessions := make([]SessionStatus, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess.status)
	}
	s.mu.Unlock()
	slices.SortFunc(sessions, func(a, b SessionStatus) int {
		if a.SessionID < b.SessionID {
			return -1
		}
		if a.SessionID > b.SessionID {
			return 1
		}
		return 0
	})
	return c.WriteResponse(&Response{
		ID:   req.ID,
		Type: TypeStatusResult,
		Status: &StatusResponse{
			ProtocolVersion: Version,
			ActiveAgents:    len(sessions),
			WarmPool:        0,
			Sessions:        sessions,
		},
	}) == nil
}

func (s *Server) handleTail(c *Conn, req *Request) bool {
	sub := &tailSub{ch: make(chan EventMessage, tailBuffer), conn: c}
	s.mu.Lock()
	s.tails = append(s.tails, sub)
	s.mu.Unlock()
	// The ack is written after the subscription is live so the first event
	// cannot race the caller learning it is subscribed.
	if err := c.WriteResponse(&Response{ID: req.ID, Type: TypeTailing, Tailing: &TailResponse{}}); err != nil {
		s.dropTail(sub)
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.writeTail(sub)
	}()
	return true
}

func (s *Server) writeTail(sub *tailSub) {
	for ev := range sub.ch {
		ev := ev
		if err := sub.conn.WriteResponse(&Response{Type: TypeEvent, Event: &ev}); err != nil {
			s.dropTail(sub)
			return
		}
	}
}

func (s *Server) emit(ev EventMessage) {
	s.mu.Lock()
	var lagged []*tailSub
	for _, sub := range s.tails {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			lagged = append(lagged, sub)
		}
	}
	s.mu.Unlock()
	for _, sub := range lagged {
		_ = replyErr(sub.conn, "", &ProtocolError{
			Code: CodeTailLagged,
			Msg:  "tail subscriber lagged; the stream is incomplete and the connection is closed",
		})
		s.dropTail(sub)
	}
}

func (s *Server) dropTail(sub *tailSub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.tails[:0]
	for _, t := range s.tails {
		if t == sub {
			if !t.closed {
				close(t.ch)
				t.closed = true
			}
			continue
		}
		kept = append(kept, t)
	}
	s.tails = kept
}

func (s *Server) reclaim(owner *connState) {
	s.mu.Lock()
	now := s.clock.Now()
	var evs []EventMessage
	for id := range owner.held {
		if _, ok := s.sessions[id]; ok {
			delete(s.sessions, id)
			evs = append(evs, EventMessage{Kind: EventReclaim, SessionID: id, At: now})
		}
	}
	owner.held = nil
	s.mu.Unlock()
	for _, ev := range evs {
		s.emit(ev)
	}
}

func (s *Server) dropConn(nc net.Conn) {
	s.mu.Lock()
	delete(s.conns, nc)
	s.mu.Unlock()
}

func (s *Server) noteRequest() {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
}

func replyErr(c *Conn, id string, err error) bool {
	pe, ok := err.(*ProtocolError)
	if !ok {
		pe = &ProtocolError{Code: CodeMalformed, Msg: err.Error()}
	}
	if pe.ID == "" {
		pe.ID = id
	}
	return c.WriteResponse(&Response{ID: pe.ID, Type: TypeError, Error: pe.Wire()}) == nil
}
