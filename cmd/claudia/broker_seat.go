// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// Seat commands speak the grant protocol the library already speaks
// (🎯T2.10): grant, send, interrupt, the per-grant agent_event stream,
// and release. There is no wire "wait". --wait folds that stream the
// way Agent.WaitForResponse does, on this process.
//
// Ownership and the event stream belong to the connection. Each command
// dials once, does its calls on that connection, and closes it. Closing
// detaches: the seat keeps running unless the command released it with
// stop. send, interrupt and events re-grant a seat the daemon already
// lists, the same reclaim a library handle does after its connection
// dropped, keeping the session id the daemon reports.

const (
	// defaultSeatTimeout bounds grant, send and --wait together. Claude
	// readiness alone can take most of a minute on a busy host; the wait
	// then has to cover the turn.
	defaultSeatTimeout = 5 * time.Minute
	// grantRPCTimeout bounds the reclaim at the start of `events`, which
	// otherwise follows the stream until interrupted.
	grantRPCTimeout = 2 * time.Minute
)

// turnSettle is how long --wait lingers after a terminal assistant event
// before treating the turn as complete. A turn can arrive as several
// assistant blocks that each carry end_turn; WaitForResponse uses the
// same settle (waitSettleDuration, 250ms) and this matches it.
var turnSettle = 250 * time.Millisecond

func grantCmd(args []string) error {
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: claudia broker grant --name NAME --provider PROVIDER [--workdir DIR] [--purpose work|aside|overseer] [--parent NAME] [--model M] [--session ID] [--adopt] [--fallback] [--send TEXT] [--mode submit|steer|interrupt|queue] [--wait] [--release stop|detach] [--timeout D] [--json]\n")
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "grant name (required; a single positional name is also accepted)")
	provider := fs.String("provider", "", "provider: claude, codex, grok, cursor, bedrock, ollama")
	workdir := fs.String("workdir", ".", "working directory the seat runs in")
	purpose := fs.String("purpose", "", "work, aside, or overseer (default work)")
	parent := fs.String("parent", "", "parent seat name (fleet lineage)")
	model := fs.String("model", "", "model override")
	session := fs.String("session", "", "session id; empty lets the daemon mint one, a known id reclaims")
	adopt := fs.Bool("adopt", false, "prefer reattaching a provider process the daemon can find")
	fallback := fs.Bool("fallback", false, "if --adopt fails, start the seat")
	sendText := fs.String("send", "", "user text to deliver on this connection after the grant")
	mode := fs.String("mode", "", "send mode when --send is set: submit (default), steer, interrupt, queue")
	wait := fs.Bool("wait", false, "fold events until the turn completes and print the assistant text")
	releaseFlag := fs.String("release", "", "after the operation, release with stop or detach")
	timeout := fs.Duration("timeout", defaultSeatTimeout, "bound for the grant, the send and --wait")
	asJSON := fs.Bool("json", false, "print one JSON object on stdout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	grantName := strings.TrimSpace(*name)
	switch {
	case grantName == "" && fs.NArg() == 1:
		grantName = fs.Arg(0)
	case grantName != "" && fs.NArg() > 0:
		return errors.New("grant: pass the name with --name or as one argument, not both")
	case fs.NArg() > 1:
		return errors.New("grant: unexpected arguments")
	}
	def, err := seatDefinition(grantName, *provider, *workdir, *purpose, *parent, *model, *session)
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	raw, err := claudia.EncodeGrantDefinition(def)
	if err != nil {
		return err
	}
	disp, err := parseReleaseFlag(*releaseFlag)
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	var send *broker.SendRequest
	if *sendText != "" || *mode != "" {
		if *sendText == "" {
			return errors.New("grant: --mode requires --send")
		}
		send = &broker.SendRequest{Name: grantName, Text: *sendText, Mode: broker.SendMode(*mode)}
		if err := send.Validate(); err != nil {
			return fmt.Errorf("grant: %w", err)
		}
		if *wait && send.Mode == broker.SendModeQueue {
			return errors.New("grant: --wait does not apply to --mode queue (nothing is written to the seat)")
		}
	} else if *wait {
		// Waiting without a send folds the stream from the grant, including
		// anything replayed from while the seat was unowned.
	}
	return driveSeat(seatDrive{
		grant: &broker.GrantRequest{
			Name: grantName, Def: raw, Adopt: *adopt, Fallback: *fallback,
		},
		announceGrant: true,
		send:          send, wait: *wait, release: disp, asJSON: *asJSON, timeout: *timeout,
	})
}

func sendCmd(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: claudia broker send [--mode submit|steer|interrupt|queue] [--text TEXT] [--wait] [--release stop|detach] [--timeout D] [--json] NAME [TEXT...]\n")
		fs.PrintDefaults()
	}
	mode := fs.String("mode", "", "delivery mode: submit (default), steer, interrupt, queue")
	textFlag := fs.String("text", "", "user text; otherwise the arguments after NAME are joined (- reads stdin)")
	wait := fs.Bool("wait", false, "fold events until the turn completes and print the assistant text")
	releaseFlag := fs.String("release", "", "after the send, release with stop or detach")
	timeout := fs.Duration("timeout", defaultSeatTimeout, "bound for the reclaim, the send and --wait")
	asJSON := fs.Bool("json", false, "print one JSON object on stdout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("send: grant name required")
	}
	name := fs.Arg(0)
	text, err := sendText(*textFlag, fs.Args()[1:])
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	send := &broker.SendRequest{Name: name, Text: text, Mode: broker.SendMode(*mode)}
	if err := send.Validate(); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	if *wait && send.Mode == broker.SendModeQueue {
		return errors.New("send: --wait does not apply to --mode queue (nothing is written to the seat)")
	}
	disp, err := parseReleaseFlag(*releaseFlag)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return driveSeat(seatDrive{
		reclaim: name, send: send, wait: *wait, release: disp, asJSON: *asJSON, timeout: *timeout,
	})
}

func interruptCmd(args []string) error {
	fs := flag.NewFlagSet("interrupt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: claudia broker interrupt [--timeout D] NAME\n")
		fs.PrintDefaults()
	}
	timeout := fs.Duration("timeout", defaultSeatTimeout, "bound for the reclaim and the interrupt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("interrupt: exactly one grant name")
	}
	return driveSeat(seatDrive{reclaim: fs.Arg(0), interrupt: true, timeout: *timeout})
}

func eventsCmd(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: claudia broker events [--wait] [--timeout D] NAME\n")
		fs.PrintDefaults()
	}
	wait := fs.Bool("wait", false, "fold events until the turn completes and print the assistant text")
	timeout := fs.Duration("timeout", 0, "stop after this long (default: until interrupted; --wait defaults to 5m)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("events: exactly one grant name")
	}
	bound := *timeout
	if *wait && bound == 0 {
		bound = defaultSeatTimeout
	}
	return driveSeat(seatDrive{reclaim: fs.Arg(0), wait: *wait, stream: !*wait, timeout: bound})
}

// sendText resolves --text, positional arguments, or stdin for "-".
func sendText(flagText string, args []string) (string, error) {
	if flagText != "" && len(args) > 0 {
		return "", errors.New("pass text with --text or as arguments, not both")
	}
	text := flagText
	if text == "" {
		if len(args) == 0 {
			return "", errors.New("text required (--text or arguments after the name)")
		}
		if len(args) == 1 && args[0] == "-" {
			text = "-"
		} else {
			text = strings.Join(args, " ")
		}
	}
	if text == "-" {
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		text = string(body)
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("text is empty")
	}
	return text, nil
}

func seatDefinition(name, provider, workdir, purpose, parent, model, session string) (claudia.GrantDefinition, error) {
	if strings.TrimSpace(name) == "" {
		return claudia.GrantDefinition{}, errors.New("name is required")
	}
	if err := checkProvider(provider); err != nil {
		return claudia.GrantDefinition{}, err
	}
	if err := checkPurpose(purpose); err != nil {
		return claudia.GrantDefinition{}, err
	}
	if workdir == "" {
		workdir = "."
	}
	return claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name:      name,
		Provider:  claudia.Provider(provider),
		WorkDir:   workdir,
		Purpose:   purpose,
		Parent:    parent,
		Model:     model,
		SessionID: session,
	}}, nil
}

func checkProvider(p string) error {
	switch claudia.Provider(p) {
	case claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderGrok,
		claudia.ProviderCursor, claudia.ProviderBedrock, claudia.ProviderOllama:
		return nil
	default:
		return fmt.Errorf("provider %q is not one of claude, codex, grok, cursor, bedrock, ollama", p)
	}
}

func checkPurpose(p string) error {
	switch p {
	case "", claudia.PurposeWork, claudia.PurposeAside, claudia.PurposeOverseer:
		return nil
	default:
		return fmt.Errorf("purpose %q is not one of %q, %q, %q", p,
			claudia.PurposeWork, claudia.PurposeAside, claudia.PurposeOverseer)
	}
}

func parseReleaseFlag(s string) (broker.Disposition, error) {
	switch broker.Disposition(s) {
	case "":
		return "", nil
	case broker.DispositionStop, broker.DispositionDetach:
		return broker.Disposition(s), nil
	default:
		return "", fmt.Errorf("release %q is not %q or %q", s, broker.DispositionStop, broker.DispositionDetach)
	}
}

// seatDrive is one owning connection's worth of grant-protocol calls.
type seatDrive struct {
	grant         *broker.GrantRequest
	announceGrant bool
	reclaim       string
	send          *broker.SendRequest
	interrupt     bool
	wait          bool
	stream        bool
	release       broker.Disposition
	asJSON        bool
	timeout       time.Duration
}

type seatOutcome struct {
	name          string
	announceGrant bool
	granted       *broker.GrantResponse
	sent          *broker.SentResponse
	askedMode     broker.SendMode
	text          string
	waited        bool
	interrupted   bool
	release       broker.Disposition
	asJSON        bool
}

func driveSeat(d seatDrive) error {
	if d.timeout < 0 {
		return errors.New("timeout must not be negative")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c, err := dialConn()
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	var deadline time.Time
	if d.timeout > 0 {
		deadline = time.Now().Add(d.timeout)
	}
	s := &cliConn{c: c}
	out := seatOutcome{asJSON: d.asJSON, announceGrant: d.announceGrant}
	if d.reclaim != "" {
		out.name = d.reclaim
	}

	var pushes []*broker.Response
	if d.grant != nil {
		out.name = d.grant.Name
		resp, got, err := s.call(ctx, &broker.Request{Type: broker.TypeGrant, Grant: d.grant}, deadline)
		if err != nil {
			return err
		}
		if resp.Granted == nil {
			return fmt.Errorf("broker: grant answered with %s", resp.Type)
		}
		out.granted = resp.Granted
		pushes = got
	} else if d.reclaim != "" {
		rpcDeadline := deadline
		if rpcDeadline.IsZero() {
			rpcDeadline = time.Now().Add(grantRPCTimeout)
		}
		granted, got, err := reclaimListed(ctx, s, rpcDeadline, d.reclaim)
		if err != nil {
			return err
		}
		out.granted = granted
		pushes = got
	}
	pushes, err = drainGoals(ctx, s, deadlineOr(deadline, grantRPCTimeout), pushes)
	if err != nil {
		return err
	}

	if d.stream {
		if err := emitEvents(pushes); err != nil {
			return err
		}
		err := streamEvents(ctx, s, deadline)
		if err != nil {
			return err
		}
		noteDetached(out.name)
		return nil
	}

	if d.send != nil {
		out.askedMode = d.send.Mode
		resp, got, err := s.call(ctx, &broker.Request{Type: broker.TypeSend, Send: d.send}, deadline)
		if err != nil {
			return err
		}
		if resp.Sent == nil {
			return fmt.Errorf("broker: send answered with %s", resp.Type)
		}
		out.sent = resp.Sent
		pushes, err = drainGoals(ctx, s, deadline, got)
		if err != nil {
			return err
		}
	}
	if d.interrupt {
		resp, _, err := s.call(ctx, &broker.Request{
			Type: broker.TypeInterrupt, Interrupt: &broker.NamedRequest{Name: out.name},
		}, deadline)
		if err != nil {
			return err
		}
		if resp.Type != broker.TypeInterrupted {
			return fmt.Errorf("broker: interrupt answered with %s", resp.Type)
		}
		out.interrupted = true
		pushes = nil
	}
	if d.wait {
		// Pushes here are the send's, when there was a send: events that
		// arrived ahead of the sent reply belong to the turn just
		// submitted. With no send they are the grant's, including a replay
		// of what the seat said while it was unowned. A send replaces the
		// grant's pushes, which is the same cut WaitForResponse makes: a
		// turn that ended before the submit is not the one it waits on.
		text, err := waitTurn(ctx, s, deadline, pushes)
		out.text = text
		out.waited = true
		if err != nil {
			return err
		}
	}
	if d.release != "" {
		if _, _, err := s.call(ctx, &broker.Request{
			Type: broker.TypeRelease,
			Release: &broker.ReleaseRequest{
				Name: out.name, Disposition: d.release,
			},
		}, deadline); err != nil {
			_ = reportSeat(out)
			return err
		}
		out.release = d.release
	}
	if err := reportSeat(out); err != nil {
		return err
	}
	if d.release == "" {
		noteDetached(out.name)
	}
	return nil
}

func noteDetached(name string) {
	fmt.Fprintf(os.Stderr, "detached %s (seat still running; `claudia broker release %s` stops it)\n", name, name)
}

func deadlineOr(deadline time.Time, fallback time.Duration) time.Time {
	if !deadline.IsZero() {
		return deadline
	}
	return time.Now().Add(fallback)
}

// reclaimListed takes ownership of a seat the daemon already holds. The
// definition is the fields the grants list carries (name, provider, model,
// session, workdir, purpose, parent) plus the daemon's session id, so a
// reclaim does not mint a replacement session. Fields the list does not
// show — MCP servers, sandbox policy — are not on this path; a library
// consumer re-grants the definition it originally sent.
func reclaimListed(ctx context.Context, s *cliConn, deadline time.Time, name string) (*broker.GrantResponse, []*broker.Response, error) {
	resp, _, err := s.call(ctx, &broker.Request{Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}}, deadline)
	if err != nil {
		return nil, nil, err
	}
	if resp.Grants == nil {
		return nil, nil, fmt.Errorf("broker: grants answered with %s", resp.Type)
	}
	var st *broker.GrantStatus
	for i := range resp.Grants.Grants {
		if resp.Grants.Grants[i].Name == name {
			st = &resp.Grants.Grants[i]
			break
		}
	}
	if st == nil {
		return nil, nil, fmt.Errorf("grant %s is not one the daemon holds (grant it first)", name)
	}
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name:      st.Name,
		Provider:  claudia.Provider(st.Provider),
		Model:     st.Model,
		SessionID: st.SessionID,
		WorkDir:   st.WorkDir,
		Purpose:   st.Purpose,
		Parent:    st.Parent,
	}})
	if err != nil {
		return nil, nil, err
	}
	// Adopt with fallback is the library's reclaim: keep the process when
	// it is still there, start it when it is not.
	got, pushes, err := s.call(ctx, &broker.Request{
		Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{
			Name: name, Def: raw, Adopt: true, Fallback: true,
		},
	}, deadline)
	if err != nil {
		return nil, nil, err
	}
	if got.Granted == nil {
		return nil, nil, fmt.Errorf("broker: grant answered with %s", got.Type)
	}
	return got.Granted, pushes, nil
}

func reportSeat(o seatOutcome) error {
	if o.asJSON {
		noteLifecycle(o)
		return writeSeatJSON(o)
	}
	if o.waited {
		noteLifecycle(o)
		if strings.HasSuffix(o.text, "\n") {
			fmt.Print(o.text)
		} else {
			fmt.Println(o.text)
		}
		return nil
	}
	if o.announceGrant && o.granted != nil {
		fmt.Printf("granted %s provider=%s session=%s reclaimed=%v\n",
			o.granted.Name, o.granted.Provider, o.granted.SessionID, o.granted.Reclaimed)
	}
	if o.sent != nil {
		fmt.Println(sentLine(o))
	}
	if o.interrupted {
		fmt.Printf("interrupted %s\n", o.name)
	}
	if o.release != "" {
		fmt.Printf("%s: %s\n", o.name, o.release)
	}
	return nil
}

func noteLifecycle(o seatOutcome) {
	if o.granted != nil {
		fmt.Fprintf(os.Stderr, "granted %s provider=%s session=%s reclaimed=%v\n",
			o.granted.Name, o.granted.Provider, o.granted.SessionID, o.granted.Reclaimed)
	}
	if o.sent != nil {
		fmt.Fprintln(os.Stderr, sentLine(o))
	}
	if o.interrupted {
		fmt.Fprintf(os.Stderr, "interrupted %s\n", o.name)
	}
	if o.release != "" {
		fmt.Fprintf(os.Stderr, "%s: %s\n", o.name, o.release)
	}
}

func sentLine(o seatOutcome) string {
	mode := o.sent.Mode
	if mode == "" {
		mode = o.askedMode
	}
	line := fmt.Sprintf("sent %s mode=%s", o.sent.Name, mode)
	if o.sent.Mechanism != "" {
		line += " mechanism=" + o.sent.Mechanism
	}
	return line
}

func writeSeatJSON(o seatOutcome) error {
	payload := map[string]any{}
	if o.granted != nil {
		payload["grant"] = o.granted
	}
	if o.sent != nil {
		payload["sent"] = o.sent
	}
	if o.waited {
		payload["text"] = o.text
	}
	if o.interrupted {
		payload["interrupted"] = true
	}
	if o.release != "" {
		payload["release"] = string(o.release)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", raw)
	return nil
}

// cliConn is one broker connection that correlates replies the way
// roundTrip does, and keeps the id-less pushes (agent_event and the
// rest) that a one-shot round trip never has to see. Grant ownership
// and the event stream both live on this connection.
type cliConn struct {
	c   *broker.Conn
	seq int
}

func (s *cliConn) call(ctx context.Context, req *broker.Request, deadline time.Time) (*broker.Response, []*broker.Response, error) {
	s.seq++
	req.ID = fmt.Sprintf("cli-%d", s.seq)
	if err := s.c.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	if err := s.c.WriteRequest(req); err != nil {
		return nil, nil, err
	}
	var pushes []*broker.Response
	for {
		if ctx.Err() != nil {
			return nil, pushes, ctx.Err()
		}
		if err := s.c.SetDeadline(deadline); err != nil {
			return nil, pushes, err
		}
		resp, err := s.c.ReadResponse()
		if err != nil {
			if ctx.Err() != nil {
				return nil, pushes, ctx.Err()
			}
			if isDeadline(err) {
				return nil, pushes, fmt.Errorf("timed out waiting for %s", req.Type)
			}
			return nil, pushes, err
		}
		if resp.ID == req.ID {
			if resp.Type == broker.TypeError {
				return nil, pushes, resp.Error.Err()
			}
			return resp, pushes, nil
		}
		// A push can arrive ahead of the reply (a reclaim replays events
		// before `granted`). Leave it for the caller. Answering a
		// goal_check from inside this read would consume the reply the
		// caller is waiting on.
		pushes = append(pushes, resp)
	}
}

// drainGoals answers goal_check pushes with "no check installed", which
// is what a consumer without GoalCompleteCheck tells the daemon. The
// reply's own pushes (events that landed while the verdict was in
// flight) come back for the caller to fold.
func drainGoals(ctx context.Context, s *cliConn, deadline time.Time, pushes []*broker.Response) ([]*broker.Response, error) {
	var rest []*broker.Response
	for _, p := range pushes {
		if p.Type != broker.TypeGoalCheck || p.GoalCheck == nil {
			rest = append(rest, p)
			continue
		}
		extra, err := answerGoal(ctx, s, deadline, p.GoalCheck)
		if err != nil {
			return rest, err
		}
		more, err := drainGoals(ctx, s, deadline, extra)
		rest = append(rest, more...)
		if err != nil {
			return rest, err
		}
	}
	return rest, nil
}

func answerGoal(ctx context.Context, s *cliConn, deadline time.Time, m *broker.GoalCheckMessage) ([]*broker.Response, error) {
	_, pushes, err := s.call(ctx, &broker.Request{
		Type: broker.TypeGoalVerdict,
		GoalVerdict: &broker.GoalVerdictRequest{
			Name: m.Name, CheckID: m.CheckID, Answered: false,
		},
	}, deadline)
	return pushes, err
}

func emitEvents(pushes []*broker.Response) error {
	for _, p := range pushes {
		if p.Type != broker.TypeAgentEvent || p.AgentEvent == nil || len(p.AgentEvent.Event) == 0 {
			continue
		}
		if _, err := os.Stdout.Write(p.AgentEvent.Event); err != nil {
			return err
		}
		if p.AgentEvent.Event[len(p.AgentEvent.Event)-1] != '\n' {
			if _, err := os.Stdout.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	return nil
}

func streamEvents(ctx context.Context, s *cliConn, deadline time.Time) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		var readFor time.Duration
		if !deadline.IsZero() {
			readFor = time.Until(deadline)
			if readFor <= 0 {
				return errors.New("timed out following events")
			}
		}
		resp, err := s.readFor(readFor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isDeadline(err) {
				return errors.New("timed out following events")
			}
			return err
		}
		switch resp.Type {
		case broker.TypeAgentEvent:
			if err := emitEvents([]*broker.Response{resp}); err != nil {
				return err
			}
		case broker.TypeAgentGone:
			name, reason := goneReason(resp)
			return fmt.Errorf("grant %s is gone: %s", name, reason)
		case broker.TypeAgentDetached:
			name, reason := "", "ownership dropped"
			if resp.AgentDetached != nil {
				name = resp.AgentDetached.Name
				if resp.AgentDetached.Reason != "" {
					reason = resp.AgentDetached.Reason
				}
			}
			return fmt.Errorf("grant %s detached: %s", name, reason)
		case broker.TypeGoalCheck:
			if resp.GoalCheck == nil {
				continue
			}
			extra, err := answerGoal(ctx, s, deadlineOr(deadline, grantRPCTimeout), resp.GoalCheck)
			if err != nil {
				return err
			}
			if err := emitEvents(extra); err != nil {
				return err
			}
		}
	}
}

// turnFold accumulates one assistant turn the way WaitForResponse does:
// assistant text, append-deltas concatenated, other blocks separated by
// a newline, a terminal stop reason, and an error event failing the turn.
type turnFold struct {
	b        strings.Builder
	terminal bool
	err      error
}

func (f *turnFold) text() string { return f.b.String() }

// add folds one event. resetSettle is true when this assistant event
// should restart the post-terminal linger.
func (f *turnFold) add(ev claudia.Event) (resetSettle bool) {
	if f.err != nil {
		return false
	}
	if ev.IsError {
		msg := strings.TrimSpace(ev.Text)
		if msg == "" {
			msg = "agent turn failed"
		}
		f.err = errors.New(msg)
		return false
	}
	if ev.Type != "assistant" {
		return false
	}
	if ev.Text != "" {
		if f.b.Len() > 0 && ev.PreviewUpdate != claudia.PreviewUpdateAppend {
			f.b.WriteByte('\n')
		}
		f.b.WriteString(ev.Text)
	}
	if ev.IsTerminalStop() {
		f.terminal = true
	}
	return f.terminal
}

// waitTurn reads the per-grant event stream until a terminal assistant
// turn has settled, or the deadline passes with none. initial are pushes
// already read (they arrived ahead of the send or grant reply).
func waitTurn(ctx context.Context, s *cliConn, deadline time.Time, initial []*broker.Response) (string, error) {
	var fold turnFold
	var settleUntil time.Time
	arm := func(reset bool) {
		if reset {
			settleUntil = time.Now().Add(turnSettle)
		}
	}
	var consume func(*broker.Response) error
	consume = func(resp *broker.Response) error {
		switch resp.Type {
		case broker.TypeAgentEvent:
			if resp.AgentEvent == nil {
				return nil
			}
			ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
			if err != nil {
				return err
			}
			arm(fold.add(ev))
			return fold.err
		case broker.TypeAgentGone:
			name, reason := goneReason(resp)
			return fmt.Errorf("grant %s is gone: %s", name, reason)
		case broker.TypeAgentDetached:
			name, reason := "", "ownership dropped"
			if resp.AgentDetached != nil {
				name, reason = resp.AgentDetached.Name, resp.AgentDetached.Reason
			}
			if reason == "" {
				reason = "ownership dropped"
			}
			return fmt.Errorf("grant %s detached: %s", name, reason)
		case broker.TypeGoalCheck:
			if resp.GoalCheck == nil {
				return nil
			}
			extra, err := answerGoal(ctx, s, deadlineOr(deadline, grantRPCTimeout), resp.GoalCheck)
			if err != nil {
				return err
			}
			for _, p := range extra {
				if err := consume(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, p := range initial {
		if err := consume(p); err != nil {
			return fold.text(), err
		}
	}
	for {
		if ctx.Err() != nil {
			return fold.text(), ctx.Err()
		}
		readFor := time.Duration(0)
		if !deadline.IsZero() {
			readFor = time.Until(deadline)
			if readFor <= 0 {
				if fold.terminal {
					return fold.text(), nil
				}
				return fold.text(), fmt.Errorf("timed out waiting for a turn (saw %q)", clip(fold.text(), reasonWidth))
			}
		}
		if fold.terminal {
			until := time.Until(settleUntil)
			if until <= 0 {
				return fold.text(), nil
			}
			if readFor == 0 || until < readFor {
				readFor = until
			}
		}
		resp, err := s.readFor(readFor)
		if err != nil {
			if ctx.Err() != nil {
				return fold.text(), ctx.Err()
			}
			if isDeadline(err) {
				if fold.terminal {
					return fold.text(), nil
				}
				return fold.text(), fmt.Errorf("timed out waiting for a turn (saw %q)", clip(fold.text(), reasonWidth))
			}
			return fold.text(), err
		}
		if err := consume(resp); err != nil {
			return fold.text(), err
		}
	}
}

func goneReason(resp *broker.Response) (string, string) {
	if resp.AgentGone == nil {
		return "", "provider stopped answering"
	}
	reason := resp.AgentGone.Reason
	if reason == "" {
		reason = "provider stopped answering"
	}
	return resp.AgentGone.Name, reason
}

func (s *cliConn) readFor(d time.Duration) (*broker.Response, error) {
	var deadline time.Time
	if d > 0 {
		deadline = time.Now().Add(d)
	}
	if err := s.c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	return s.c.ReadResponse()
}

func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
