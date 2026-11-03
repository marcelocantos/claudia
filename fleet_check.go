// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A check is a pinned command run on the owner's behalf (🎯T248.3). The owner
// registers it; the kernel keeps the script, pins it by hash, runs exactly those
// bytes against a fresh export of a named commit, out of every agent's reach, and
// records the result in the fleet's log, which agents cannot write. What the
// result means, the target and whether it is met belong to whoever registered
// it. A check agents may not run is a held-out check.
//
// The export comes from git archive, not a checkout: a checkout would run the
// repository's hooks and filters, which agents working in the same repository
// can write, and so could forge a result.

const (
	fleetCheckDefaultTimeout = 30 * time.Minute
	fleetCheckMaxTimeout     = 4 * time.Hour
	// fleetCheckConcurrency bounds checks running at once on the broker's host.
	fleetCheckConcurrency = 2
	// fleetCheckOutput bounds the output a result keeps: its tail.
	fleetCheckOutput = 16 << 10
	fleetCheckKill   = 5 * time.Second
	// fleetCheckQueue bounds a fleet's runs waiting or running at once.
	fleetCheckQueue = 8
	// fleetMaxLinkTarget bounds a symlink's target in an export.
	fleetMaxLinkTarget = 4096
	// fleetGitTimeout bounds every git command a check needs.
	fleetGitTimeout = 2 * time.Minute
)

// FleetCheckArgs registers or replaces a check.
type FleetCheckArgs struct {
	Name string
	// Repo is the absolute path of the git repository checked out for each run.
	Repo string
	// Script is the shell script to run, or ScriptFile names a file holding it.
	// Its contents are copied, so later edits to the file change nothing.
	Script     string
	ScriptFile string
	// AgentsMayRun lets any agent run it. Otherwise it is held out: only those
	// with authority over whoever registered it can run it or see it.
	AgentsMayRun bool
	// Timeout ends a run that takes longer. Zero is 30 minutes.
	Timeout time.Duration
}

// FleetCheckStatus describes a registered check.
type FleetCheckStatus struct {
	Name           string  `json:"name"`
	Repo           string  `json:"repo"`
	SHA256         string  `json:"sha256"`
	AgentsMayRun   bool    `json:"agents_may_run"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
	// Last is the latest recorded result, if any.
	Last *FleetCheckResult `json:"last,omitempty"`
}

// FleetCheckResult is one run, as the kernel recorded it.
type FleetCheckResult struct {
	RunID     string `json:"run_id"`
	Name      string `json:"name"`
	Commit    string `json:"commit"`
	SHA256    string `json:"sha256,omitempty"`
	Requester string `json:"requester"`
	// Exit is the script's exit status; -1 when it did not finish.
	Exit int `json:"exit"`
	// Score is the last line of standard output, when that is a number.
	Score   *float64 `json:"score,omitempty"`
	Output  string   `json:"output,omitempty"`
	Seconds float64  `json:"seconds,omitempty"`
	// Error is why the run did not produce a result.
	Error string `json:"error,omitempty"`
}

type fleetCheckState struct {
	Repo           string    `json:"repo"`
	Path           string    `json:"path"`
	SHA256         string    `json:"sha256"`
	AgentsMayRun   bool      `json:"agents_may_run,omitempty"`
	TimeoutSeconds float64   `json:"timeout_seconds"`
	Registered     time.Time `json:"registered"`
	// RegisteredBy is the owner or the agent that registered it.
	RegisteredBy string `json:"registered_by,omitempty"`
}

type fleetCheckRun struct {
	Check     string `json:"check"`
	Commit    string `json:"commit"`
	Requester string `json:"requester"`
	// RequesterToken tells the agent that asked from a later agent of the same
	// name, which must not receive the result.
	RequesterToken string    `json:"requester_token,omitempty"`
	Started        time.Time `json:"started"`
}

// RegisterCheck pins a check's script. Anyone may register a check; replacing
// one takes authority over whoever registered it, and the log records the new
// hash and who replaced it.
func (k *FleetKernel) RegisterCheck(fleet, caller string, args *FleetCheckArgs) error {
	if args == nil || !fleetNameValid(args.Name) {
		return errors.New("a check needs a name of lowercase letters, digits and single hyphens")
	}
	if !filepath.IsAbs(args.Repo) {
		return fmt.Errorf("repo %q is not absolute", args.Repo)
	}
	if _, err := fleetGit(args.Repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("repo %s: %w", args.Repo, err)
	}
	script := []byte(args.Script)
	if args.ScriptFile != "" {
		raw, err := os.ReadFile(args.ScriptFile)
		if err != nil {
			return fmt.Errorf("read check script: %w", err)
		}
		script = raw
	}
	if strings.TrimSpace(string(script)) == "" {
		return errors.New("a check needs a script")
	}
	timeout := args.Timeout
	if timeout == 0 {
		timeout = fleetCheckDefaultTimeout
	}
	if timeout < 0 || timeout > fleetCheckMaxTimeout {
		return fmt.Errorf("a check's timeout must be between 0 and %s", fleetCheckMaxTimeout)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return err
	}
	if old := group.Checks[args.Name]; old != nil && !authorized(group, caller, old.RegisteredBy) {
		return fmt.Errorf("agent %q has no authority over %q, who registered check %s", caller, old.RegisteredBy, args.Name)
	}
	dir := filepath.Join(k.dir, "checks", fleet)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, args.Name+".sh")
	_ = os.Remove(path)
	if err := os.WriteFile(path, script, 0o400); err != nil {
		return fmt.Errorf("pin check script: %w", err)
	}
	sum := sha256.Sum256(script)
	if group.Checks == nil {
		group.Checks = map[string]*fleetCheckState{}
	}
	// A new script's results start afresh; an old one's never stand for it.
	delete(group.CheckLast, args.Name)
	group.Checks[args.Name] = &fleetCheckState{Repo: args.Repo, Path: path, SHA256: hex.EncodeToString(sum[:]),
		AgentsMayRun: args.AgentsMayRun, TimeoutSeconds: timeout.Seconds(), Registered: k.clock.Now(), RegisteredBy: caller}
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventCheckRegistered, Agent: caller, Text: fmt.Sprintf("%s %s %s", args.Name, hex.EncodeToString(sum[:]), args.Repo)})
	return k.saveLocked()
}

// RunCheck starts a run and returns its ID. The result lands in the log, and an
// agent that asked also gets it as a message. commit is any revision the repo
// can resolve; empty means HEAD.
func (k *FleetKernel) RunCheck(fleet, requester, name, commit string) (string, error) {
	if commit == "" {
		commit = "HEAD"
	}
	if strings.HasPrefix(commit, "-") || strings.ContainsAny(commit, " \t\r\n") {
		return "", fmt.Errorf("%q is not a revision", commit)
	}
	k.mu.Lock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		k.mu.Unlock()
		return "", err
	}
	check := group.Checks[name]
	if check == nil || (!check.AgentsMayRun && !authorized(group, requester, check.RegisteredBy)) {
		k.mu.Unlock()
		return "", fmt.Errorf("no check named %q", name)
	}
	if requester != FleetOwner && group.Agents[requester] == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("no agent named %q", requester)
	}
	if requester != FleetOwner {
		queued := 0
		for _, run := range group.CheckRuns {
			if run.Requester != FleetOwner {
				queued++
			}
		}
		if queued >= fleetCheckQueue {
			k.mu.Unlock()
			return "", fmt.Errorf("%d agents' checks are already waiting or running; wait for a result", fleetCheckQueue)
		}
	}
	repo := check.Repo
	k.mu.Unlock()

	sha, err := fleetGit(repo, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("revision %s: %w", commit, err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	group, err = k.groupLocked(fleet)
	if err != nil {
		return "", err
	}
	// Checked again: the check may have been replaced while git ran.
	check = group.Checks[name]
	if check == nil || (!check.AgentsMayRun && !authorized(group, requester, check.RegisteredBy)) {
		return "", fmt.Errorf("no check named %q", name)
	}
	group.NextID++
	id := fmt.Sprintf("c%d", group.NextID)
	run := &fleetCheckRun{Check: name, Commit: sha, Requester: requester, Started: k.clock.Now()}
	if a := group.Agents[requester]; a != nil {
		run.RequesterToken = a.Token
	}
	if group.CheckRuns == nil {
		group.CheckRuns = map[string]*fleetCheckRun{}
	}
	group.CheckRuns[id] = run
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventCheckStarted, Agent: requester, Check: &FleetCheckResult{RunID: id, Name: name, Commit: sha, Requester: requester, Exit: -1}})
	if err := k.saveLocked(); err != nil {
		delete(group.CheckRuns, id)
		return "", err
	}
	pinned := *check
	ctx, cancel := context.WithCancel(k.ctx)
	k.checkSeq++
	key := fmt.Sprintf("%s#%d", fleetKey(fleet, id), k.checkSeq)
	k.checkCancels[key] = cancel
	k.wg.Add(1)
	go k.executeCheck(ctx, key, fleet, id, run, &pinned)
	return id, nil
}

// executeCheck runs one check and records what happened. If the kernel closes
// first, the run stays recorded, and the next kernel reports it interrupted. A
// fleet shut down meanwhile cancels it, and its result goes nowhere.
func (k *FleetKernel) executeCheck(ctx context.Context, key, fleet, id string, run *fleetCheckRun, check *fleetCheckState) {
	defer k.wg.Done()
	defer func() {
		k.mu.Lock()
		if cancel := k.checkCancels[key]; cancel != nil {
			cancel()
			delete(k.checkCancels, key)
		}
		k.mu.Unlock()
	}()
	select {
	case k.checkSlots <- struct{}{}:
		defer func() { <-k.checkSlots }()
	case <-ctx.Done():
		return
	}
	result := &FleetCheckResult{RunID: id, Name: run.Check, Commit: run.Commit, SHA256: check.SHA256, Requester: run.Requester, Exit: -1}
	started := time.Now()
	runCheckProcess(ctx, filepath.Join(k.dir, "checkouts"), id, check, run.Commit, result)
	result.Seconds = time.Since(started).Seconds()
	if ctx.Err() != nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.finishCheckLocked(fleet, id, result)
	if err := k.saveLocked(); err != nil {
		k.logf("fleet: save after check", "fleet", fleet, "err", err)
	}
}

// finishCheckLocked records a result and tells the agent that asked, if it is
// still the same agent. k.mu held.
func (k *FleetKernel) finishCheckLocked(fleet, id string, result *FleetCheckResult) {
	group := k.state.Fleets[fleet]
	if group == nil {
		return
	}
	run := group.CheckRuns[id]
	delete(group.CheckRuns, id)
	result.Output = boundCheckOutput(result.Output)
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventCheckResult, Agent: result.Requester, Check: result})
	if group.CheckLast == nil {
		group.CheckLast = map[string]*FleetCheckResult{}
	}
	if current := group.Checks[result.Name]; current != nil && current.SHA256 == result.SHA256 {
		group.CheckLast[result.Name] = result
	}
	a := group.Agents[result.Requester]
	if result.Requester == FleetOwner || a == nil || run == nil || a.Token != run.RequesterToken {
		return
	}
	k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: FleetCheckSenderPrefix + result.Name, To: result.Requester,
		InReplyTo: id, Text: describeCheckResult(result)})
}

// reportInterruptedChecksLocked answers runs a restart cut short. k.mu held.
func (k *FleetKernel) reportInterruptedChecksLocked(fleet string, group *fleetGroupState) {
	for id, run := range group.CheckRuns {
		k.finishCheckLocked(fleet, id, &FleetCheckResult{RunID: id, Name: run.Check, Commit: run.Commit, Requester: run.Requester,
			Exit: -1, Error: "the broker restarted during this run; run it again"})
	}
}

func describeCheckResult(r *FleetCheckResult) string {
	commit := r.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Check %s on %s", r.Name, commit)
	switch {
	case r.Error != "":
		fmt.Fprintf(&b, " produced no result: %s.", r.Error)
	case r.Score != nil:
		fmt.Fprintf(&b, ": score %s, exit %d, after %s.", strconv.FormatFloat(*r.Score, 'g', -1, 64), r.Exit, fleetDuration(time.Duration(r.Seconds*float64(time.Second))))
	default:
		fmt.Fprintf(&b, ": exit %d, after %s.", r.Exit, fleetDuration(time.Duration(r.Seconds*float64(time.Second))))
	}
	if r.Output != "" {
		b.WriteString("\n\nOutput (tail):\n" + r.Output)
	}
	return b.String()
}

// runCheckProcess runs the pinned script against a fresh export of commit, in
// its own process group, and fills in result. The bytes run are the bytes whose
// hash was just verified, so swapping the file afterwards changes nothing.
// Everything the script started is killed when it ends.
func runCheckProcess(ctx context.Context, checkouts, id string, check *fleetCheckState, commit string, result *FleetCheckResult) {
	script, err := os.ReadFile(check.Path)
	if err != nil {
		result.Error = "the pinned script is unreadable: " + err.Error()
		return
	}
	sum := sha256.Sum256(script)
	if hex.EncodeToString(sum[:]) != check.SHA256 {
		result.Error = "the pinned script was modified after it was registered"
		return
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	dir := filepath.Join(checkouts, id+"-"+hex.EncodeToString(suffix))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		result.Error = err.Error()
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// The export spends from the same timeout as the script, so a large
	// repository gets the time its owner allowed.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	if err := exportCommit(ctx, check.Repo, commit, dir); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.Error = fmt.Sprintf("timed out after %s while exporting the commit", fleetDuration(time.Duration(check.TimeoutSeconds*float64(time.Second))))
		} else {
			result.Error = "export: " + err.Error()
		}
		return
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", string(script), "check")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CLAUDIA_CHECK_COMMIT="+commit, "CLAUDIA_CHECK_REPO="+check.Repo)
	setCheckProcessGroup(cmd)
	cmd.WaitDelay = fleetCheckKill
	stdout := &tailBuffer{limit: fleetCheckOutput}
	combined := &tailBuffer{limit: fleetCheckOutput}
	cmd.Stdout = teeWriter{stdout, combined}
	cmd.Stderr = combined
	runErr := cmd.Run()
	killCheckProcessGroup(cmd)
	result.Output = combined.String()
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result.Error = fmt.Sprintf("timed out after %s", fleetDuration(time.Duration(check.TimeoutSeconds*float64(time.Second))))
		return
	case runErr == nil:
		result.Exit = 0
	case errors.As(runErr, &exitErr):
		result.Exit = exitErr.ExitCode()
	case errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The script finished, but something it started still held its output.
		result.Exit = cmd.ProcessState.ExitCode()
	default:
		result.Error = runErr.Error()
		return
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		if score, err := strconv.ParseFloat(last, 64); err == nil {
			result.Score = &score
		}
	}
}

// exportCommit writes commit's tree into dir by walking its objects. A git
// archive would honour the shared repository's attributes and replace refs,
// both of which agents can write, so either could forge a result. ls-tree and
// cat-file read the objects themselves, with replacement turned off. Files are
// written through an os.Root, which no path or symlink can lead out of; no file
// is written twice; and symlinks are made last, so none can redirect a write.
func exportCommit(ctx context.Context, repo, commit, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	listing, err := fleetGitContext(ctx, repo, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return err
	}
	type entry struct{ mode, oid, path string }
	var files, links []entry
	for _, record := range strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return fmt.Errorf("unexpected ls-tree record %q", record)
		}
		e := entry{mode: fields[0], oid: fields[2], path: path}
		switch {
		case fields[1] != "blob":
			// A submodule is a pointer to another repository, not content here.
		case e.mode == "120000":
			links = append(links, e)
		default:
			files = append(files, e)
		}
	}

	// Its own context, so that any early return also ends git: a git process
	// left writing into a pipe nobody reads would hold the check slot until
	// the check's timeout.
	gitCtx, stopGit := context.WithCancel(ctx)
	defer stopGit()
	cmd := fleetGitCommand(gitCtx, repo, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		stopGit()
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	objects := bufio.NewReader(stdout)
	// open asks for a blob and returns its size, leaving the content next to read.
	open := func(oid string) (int64, error) {
		if _, err := io.WriteString(stdin, oid+"\n"); err != nil {
			return 0, err
		}
		header, err := objects.ReadString('\n')
		if err != nil {
			return 0, err
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return 0, fmt.Errorf("object %s: %q", oid, strings.TrimSpace(header))
		}
		return strconv.ParseInt(fields[2], 10, 64)
	}
	// finish consumes the newline after an object's content.
	finish := func() error {
		_, err := objects.Discard(1)
		return err
	}
	for _, e := range files {
		size, err := open(e.oid)
		if err != nil {
			return err
		}
		if parent := filepath.Dir(e.path); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", e.path, err)
			}
		}
		perm := os.FileMode(0o644)
		if e.mode == "100755" {
			perm = 0o755
		}
		f, err := root.OpenFile(e.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if errors.Is(err, os.ErrExist) {
			// Paths that differ only in case collide on a case-insensitive
			// filesystem. Keeping either would let whoever wrote the commit choose
			// what the check reads, so the run fails instead.
			return fmt.Errorf("%s collides with another path on this filesystem", e.path)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", e.path, err)
		}
		// Streamed, never held whole: a huge file compresses to almost nothing
		// in git, and would otherwise cost the broker its size in memory.
		_, err = io.CopyN(f, objects, size)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", e.path, err)
		}
		if err := finish(); err != nil {
			return err
		}
	}
	for _, e := range links {
		size, err := open(e.oid)
		if err != nil {
			return err
		}
		if size > fleetMaxLinkTarget {
			return fmt.Errorf("%s: a link target of %d bytes", e.path, size)
		}
		target := make([]byte, size)
		if _, err := io.ReadFull(objects, target); err != nil {
			return err
		}
		if err := finish(); err != nil {
			return err
		}
		// A link may point only within the export, so that the owner's script
		// cannot be led to read or write anything outside it.
		if filepath.IsAbs(string(target)) || !filepath.IsLocal(filepath.Join(filepath.Dir(e.path), string(target))) {
			return fmt.Errorf("%s: the link target %q leaves the export", e.path, target)
		}
		if parent := filepath.Dir(e.path); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", e.path, err)
			}
		}
		if err := root.Symlink(string(target), e.path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s collides with another path on this filesystem", e.path)
			}
			return fmt.Errorf("%s: %w", e.path, err)
		}
	}
	return nil
}

func boundCheckOutput(s string) string {
	if len(s) <= fleetCheckOutput {
		return s
	}
	return s[len(s)-fleetCheckOutput:]
}

// tailBuffer keeps the last limit bytes written to it. A process's standard
// output and error are copied on separate goroutines, so it locks.
type tailBuffer struct {
	limit int
	mu    sync.Mutex
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.limit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) > t.limit {
		return string(t.buf[len(t.buf)-t.limit:])
	}
	return string(t.buf)
}

type teeWriter struct{ a, b *tailBuffer }

func (w teeWriter) Write(p []byte) (int, error) {
	_, _ = w.a.Write(p)
	return w.b.Write(p)
}

// fleetGit runs a git query for a check, bounded in time.
func fleetGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fleetGitTimeout)
	defer cancel()
	return fleetGitContext(ctx, dir, args...)
}

// fleetGitCommand is git as a check uses it: hooks off, and replace refs and
// grafts ignored, since agents working in the repository can write all three.
func fleetGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "advice.graftFileDeprecated=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE=/dev/null")
	return cmd
}

// fleetGitContext returns standard output only, so that git's advice on
// standard error can never end up inside a result.
func fleetGitContext(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := fleetGitCommand(ctx, dir, args...)
	var stderr tailBuffer
	stderr.limit = fleetCheckOutput
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *FleetKernel) checkStatusLocked(group *fleetGroupState, agentsOnly bool) []FleetCheckStatus {
	out := make([]FleetCheckStatus, 0, len(group.Checks))
	for name, c := range group.Checks {
		if agentsOnly && !c.AgentsMayRun {
			continue
		}
		out = append(out, FleetCheckStatus{Name: name, Repo: c.Repo, SHA256: c.SHA256, AgentsMayRun: c.AgentsMayRun, TimeoutSeconds: c.TimeoutSeconds, Last: group.CheckLast[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
