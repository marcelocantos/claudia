// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"archive/tar"
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
	// AgentsMayRun lets agents run it with the check tool. Otherwise only the
	// owner can, and agents never see its results.
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

// RegisterCheck pins a check's script. Re-registering a name replaces it, and
// the log records the new hash. Only the owner can do this.
func (k *FleetKernel) RegisterCheck(fleet string, args *FleetCheckArgs) error {
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
	group.Checks[args.Name] = &fleetCheckState{Repo: args.Repo, Path: path, SHA256: hex.EncodeToString(sum[:]),
		AgentsMayRun: args.AgentsMayRun, TimeoutSeconds: timeout.Seconds(), Registered: k.clock.Now()}
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventCheckRegistered, Text: fmt.Sprintf("%s %s %s", args.Name, hex.EncodeToString(sum[:]), args.Repo)})
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
	if check == nil || (requester != FleetOwner && !check.AgentsMayRun) {
		k.mu.Unlock()
		return "", fmt.Errorf("no check named %q", name)
	}
	if requester != FleetOwner && group.Agents[requester] == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("no agent named %q", requester)
	}
	if len(group.CheckRuns) >= fleetCheckQueue {
		k.mu.Unlock()
		return "", fmt.Errorf("%d checks are already waiting or running; wait for a result", fleetCheckQueue)
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
	check = group.Checks[name]
	if check == nil {
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
	k.checkCancels[fleetKey(fleet, id)] = cancel
	k.wg.Add(1)
	go k.executeCheck(ctx, fleet, id, run, &pinned)
	return id, nil
}

// executeCheck runs one check and records what happened. If the kernel closes
// first, the run stays recorded, and the next kernel reports it interrupted. A
// fleet shut down meanwhile cancels it, and its result goes nowhere.
func (k *FleetKernel) executeCheck(ctx context.Context, fleet, id string, run *fleetCheckRun, check *fleetCheckState) {
	defer k.wg.Done()
	defer func() {
		k.mu.Lock()
		if cancel := k.checkCancels[fleetKey(fleet, id)]; cancel != nil {
			cancel()
			delete(k.checkCancels, fleetKey(fleet, id))
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
	if err := exportCommit(ctx, check.Repo, commit, dir); err != nil {
		result.Error = "export: " + err.Error()
		return
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds*float64(time.Second)))
	defer cancel()
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

// exportCommit writes commit's tree into dir with git archive, which runs no
// hooks, filters or downloads. Paths that would leave dir are refused.
func exportCommit(ctx context.Context, repo, commit, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, fleetGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "-c", "core.hooksPath=/dev/null", "archive", "--format=tar", commit)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	stderr.limit = fleetCheckOutput
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractTar(out, dir)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

func extractTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q leaves the export", h.Name)
		}
		path := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, path); err != nil {
				return err
			}
		}
	}
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

// fleetGit runs a git query for a check, bounded in time and with hooks off.
func fleetGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fleetGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *FleetKernel) checkStatusLocked(group *fleetGroupState, agentsOnly bool) []FleetCheckStatus {
	out := make([]FleetCheckStatus, 0, len(group.Checks))
	for name, c := range group.Checks {
		if agentsOnly && !c.AgentsMayRun {
			continue
		}
		out = append(out, FleetCheckStatus{Name: name, Repo: c.Repo, SHA256: c.SHA256, AgentsMayRun: c.AgentsMayRun, TimeoutSeconds: c.TimeoutSeconds})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
