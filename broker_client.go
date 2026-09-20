// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The library's side of the socket (🎯T3). One brokerClient is one
// connection: a request/response channel correlated by envelope id, plus a
// push stream (agent_event, agent_term, agent_gone, task_event, task_done)
// that arrives without an id and is handed to the owner's push callback.
//
// A grant owns its connection for the life of the Agent; a task run owns one
// for the life of the run; a usage read opens and closes one. Connection
// identity is what the daemon uses for ownership, so a client never shares a
// connection between two seats.

// errNoBroker means no daemon is reachable: the socket is absent, the
// process is the daemon, or CLAUDIA_NO_BROKER is set. Callers take the
// direct path.
var errNoBroker = errors.New("claudia: no broker")

// errBrokerNotAvailable means a bare protocol server answered but has no
// daemon runtime behind it. Callers take the direct path.
var errBrokerNotAvailable = errors.New("claudia: broker has no daemon runtime")

// brokerDialTimeout bounds the connect plus the first round trip. A socket
// whose owner has hung must not stall every Start on the host.
const brokerDialTimeout = 2 * time.Second

// brokerClient is one framed connection to the daemon.
type brokerClient struct {
	conn *broker.Conn
	seq  atomic.Int64

	mu      sync.Mutex
	pending map[string]chan *broker.Response
	push    func(*broker.Response)
	closed  bool
	err     error
	done    chan struct{}

	// dropped counts frames skipped for exceeding the wire's line limit, and
	// lastDrop is the most recent refusal. A skipped frame is a hole in the
	// event stream, not a closed connection (🎯T73).
	dropped  int
	lastDrop error
}

// usingBroker reports whether this process may talk to a lifecycle broker.
func usingBroker() bool {
	return !broker.Disabled() && !broker.SelfHosted()
}

// dialBroker connects to the daemon. errNoBroker when there is none to dial.
func dialBroker() (*brokerClient, error) {
	if !usingBroker() {
		return nil, errNoBroker
	}
	path, err := broker.SocketPath()
	if err != nil {
		return nil, errNoBroker
	}
	c, err := broker.Dial(path)
	if err != nil {
		return nil, errNoBroker
	}
	bc := &brokerClient{conn: c, pending: map[string]chan *broker.Response{}, done: make(chan struct{})}
	go bc.readLoop()
	return bc, nil
}

// setPush installs the handler for id-less messages. Must be set before the
// request that starts a stream is sent.
func (b *brokerClient) setPush(fn func(*broker.Response)) {
	b.mu.Lock()
	b.push = fn
	b.mu.Unlock()
}

func (b *brokerClient) readLoop() {
	defer close(b.done)
	for {
		resp, err := b.conn.ReadResponse()
		if err != nil {
			if errors.Is(err, broker.ErrFrameTooLarge) {
				// One frame was too large to relay and the broker skipped it.
				// Everything else on this connection — every pending request,
				// every other seat's events — is unaffected, so the loop keeps
				// reading. Failing here is the 🎯T661 defect itself: a 1.2 KB
				// send died because a screenshot shared its connection.
				b.noteDropped(err)
				continue
			}
			b.fail(err)
			return
		}
		if resp.ID == "" {
			b.mu.Lock()
			push := b.push
			b.mu.Unlock()
			if push != nil {
				push(resp)
			}
			continue
		}
		b.mu.Lock()
		ch, ok := b.pending[resp.ID]
		if ok {
			delete(b.pending, resp.ID)
		}
		b.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// noteDropped records a frame the wire could not carry, so that a hole in the
// event stream is a fact this package holds rather than one only a log line
// remembers: a silently missing tool_result is indistinguishable from one the
// agent never produced.
//
// brokerClient is unexported, so this count does not reach a consumer of the
// library today — the oracles read it, and nothing else can. Saying otherwise
// would be the more comfortable comment and the false one. Handing it out is a
// public API decision that 🎯T73 did not ask for and should not smuggle in.
func (b *brokerClient) noteDropped(err error) {
	b.mu.Lock()
	b.dropped++
	b.lastDrop = err
	b.mu.Unlock()
}

// DroppedFrames reports how many frames this connection skipped because they
// exceeded the wire's line limit, and the last such refusal. It is in-package
// only; see noteDropped.
func (b *brokerClient) DroppedFrames() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped, b.lastDrop
}

// fail closes the client and wakes every waiter with err.
func (b *brokerClient) fail(err error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	if err == nil || err == io.EOF {
		err = errors.New("claudia: broker connection closed")
	}
	b.err = err
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()
	_ = b.conn.Close()
	for _, ch := range pending {
		close(ch)
	}
}

// Close drops the connection. A grant's seat keeps running on the daemon
// (detach); a task run is cancelled.
func (b *brokerClient) Close() {
	b.fail(errors.New("claudia: broker client closed"))
}

// call sends req and waits for the response carrying its id. A TypeError
// answer comes back as a *broker.ProtocolError; CodeNotAvailable is mapped to
// errBrokerNotAvailable so callers can fall back to the direct path.
func (b *brokerClient) call(ctx context.Context, req *broker.Request) (*broker.Response, error) {
	id := "c" + strconv.FormatInt(b.seq.Add(1), 10)
	req.ID = id
	ch := make(chan *broker.Response, 1)
	b.mu.Lock()
	if b.closed {
		err := b.err
		b.mu.Unlock()
		return nil, err
	}
	b.pending[id] = ch
	b.mu.Unlock()
	if err := b.conn.WriteRequest(req); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			b.mu.Lock()
			err := b.err
			b.mu.Unlock()
			return nil, err
		}
		if resp.Type == broker.TypeError {
			pe := resp.Error.Err()
			if pe.Code == broker.CodeNotAvailable {
				return nil, fmt.Errorf("%w: %s", errBrokerNotAvailable, pe.Msg)
			}
			return nil, pe
		}
		return resp, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// callTimeout is call with a deadline for one-shot reads.
func (b *brokerClient) callTimeout(req *broker.Request, d time.Duration) (*broker.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return b.call(ctx, req)
}

// BrokerAvailable reports whether a claudia daemon is reachable from this
// process: the socket answers and a daemon runtime (not a bare protocol
// server) is behind it. Consumers use it to decide who owns agent survival
// across their own restart (🎯T2.11).
func BrokerAvailable() bool {
	bc, err := dialBroker()
	if err != nil {
		return false
	}
	defer bc.Close()
	_, err = bc.callTimeout(&broker.Request{Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}}, brokerDialTimeout)
	return err == nil
}
