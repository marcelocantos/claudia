// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command claudia is the daemon and its operator CLI (🎯T2, 🎯T2.7).
//
//	claudia broker serve            run the daemon in the foreground
//	claudia broker status           what it holds
//	claudia broker grants           every seat, owner and liveness
//	claudia broker tail             lifecycle events as NDJSON
//	claudia broker usage [--refresh] the plan-usage snapshot
//	claudia broker grant            start or reclaim a named seat
//	claudia broker send             deliver a turn (submit, steer, or interrupt-then-submit)
//	claudia broker interrupt        cancel the seat's current turn
//	claudia broker events           follow one seat's event stream
//	claudia broker release NAME [--detach | --force]
//	claudia broker install|uninstall  launchd user agent (macOS)
//	claudia broker socket           print the socket path
//	claudia models intel …          purpose-quality series (🎯T71)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/daemon"
	"github.com/marcelocantos/claudia/internal/broker"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "broker":
		err = brokerCmd(args[1:])
	case "models":
		err = modelsCmd(args[1:])
	case "version", "--version", "-v":
		fmt.Println(claudia.Version)
	case "-h", "--help", "help":
		usage()
	case "--help-agent":
		fmt.Print(usageText())
		fmt.Print(claudia.AgentsGuide)
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claudia:", err)
		return 1
	}
	return 0
}

func usageText() string {
	return `usage: claudia broker <serve|status|grants|tail|usage|release|grant|send|interrupt|events|install|uninstall|socket> [flags]
       claudia models intel <refresh|latest|history|drift> [flags]
       claudia version | --version | -v
       claudia --help | -h
       claudia --help-agent
`
}

func usage() {
	fmt.Fprint(os.Stderr, usageText())
}

func brokerCmd(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("broker: subcommand required")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "status":
		return status()
	case "grants":
		return grants()
	case "tail":
		return tail()
	case "usage":
		return usageCmd(args[1:])
	case "release":
		return release(args[1:])
	case "grant":
		return grantCmd(args[1:])
	case "send":
		return sendCmd(args[1:])
	case "interrupt":
		return interruptCmd(args[1:])
	case "events":
		return eventsCmd(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	case "install":
		return install(args[1:])
	case "uninstall":
		return uninstall()
	case "socket":
		p, err := broker.SocketPath()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	default:
		usage()
		return fmt.Errorf("broker: unknown subcommand %q", args[0])
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "grants and logs directory (default: claudia state dir)")
	socket := fs.String("socket", "", "socket path (default: CLAUDIA_BROKER_SOCKET or the state dir)")
	usageTTL := fs.Duration("usage-ttl", 0, "plan-usage refresh interval (default 5m)")
	noResume := fs.Bool("no-resume", false, "do not bring back seats held before the last stop")
	nudge := fs.String("restart-nudge", "", "message for seats relaunched on boot; '-' sends none")
	logLevel := fs.String("log", "info", "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("--log: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	// This process IS the daemon: its own Start/Task/usage calls take the
	// direct path, and its provider children inherit nothing that would
	// send their claudia consumers around it.
	broker.MarkSelfHosted()

	d, err := daemon.New(daemon.Options{
		SocketPath:    *socket,
		StateDir:      *stateDir,
		UsageTTL:      *usageTTL,
		DisableResume: *noResume,
		RestartNudge:  *nudge,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = d.Run(ctx)
	log.Info("claudia broker stopped")
	return err
}

// cliRPCTimeout bounds one status-style round trip. A seat command keeps
// the connection open and applies its own --timeout; grant startup can
// outlast this.
const cliRPCTimeout = 30 * time.Second

// dialConn connects to the daemon or explains why it cannot. The caller
// sets any deadline: a one-shot round trip wants one, a seat command
// that then waits on events does not.
func dialConn() (*broker.Conn, error) {
	path, err := broker.SocketPath()
	if err != nil {
		return nil, err
	}
	c, err := broker.Dial(path)
	if err != nil {
		return nil, fmt.Errorf("no daemon at %s (start one with `claudia broker serve` or `claudia broker install`): %w", path, err)
	}
	return c, nil
}

// dial connects to the daemon for one short round trip.
func dial() (*broker.Conn, error) {
	c, err := dialConn()
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(cliRPCTimeout))
	return c, nil
}

func roundTrip(req *broker.Request) (*broker.Response, error) {
	c, err := dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	req.ID = "cli"
	if err := c.WriteRequest(req); err != nil {
		return nil, err
	}
	resp, err := c.ReadResponse()
	if err != nil {
		return nil, err
	}
	if resp.Type == broker.TypeError {
		return nil, resp.Error.Err()
	}
	return resp, nil
}

func status() error {
	resp, err := roundTrip(&broker.Request{Type: broker.TypeStatus, Status: &broker.StatusRequest{}})
	if err != nil {
		return err
	}
	st := resp.Status
	fmt.Printf("protocol v%d  grants %d  tasks %d", st.ProtocolVersion, len(st.Grants), st.Tasks)
	if st.UsageFetchedAt.IsZero() {
		fmt.Print("  usage: not fetched")
	} else {
		fmt.Printf("  usage: %s ago", time.Since(st.UsageFetchedAt).Round(time.Second))
	}
	fmt.Println()
	if !st.UsageFetchedAt.IsZero() {
		if resp, err := roundTrip(&broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{}}); err == nil {
			var backends []claudia.PlanUsage
			if json.Unmarshal(resp.Usage.Backends, &backends) == nil {
				var parts []string
				for _, b := range backends {
					if b.Status != claudia.PlanUsageAvailable {
						continue
					}
					band := claudia.ClassifyPlan(b, time.Now(), nil)
					p := fmt.Sprintf("%s=%s", b.Provider, band.Weekly)
					for _, w := range b.Windows {
						if w.Name == claudia.PlanWindowWeekly && w.RemainingPercent != nil {
							p += fmt.Sprintf("(%.0f%% left)", *w.RemainingPercent)
						}
					}
					parts = append(parts, p)
				}
				if len(parts) > 0 {
					fmt.Println("bands:", strings.Join(parts, "  "))
				}
			}
		}
	}
	return printGrants(st.Grants)
}

func grants() error {
	resp, err := roundTrip(&broker.Request{Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
	if err != nil {
		return err
	}
	return printGrants(resp.Grants.Grants)
}

func printGrants(gs []broker.GrantStatus) error {
	if len(gs) == 0 {
		fmt.Println("no seats")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tMODEL\tOWNED\tOWNER\tALIVE\tPENDING\tPURPOSE\tPARENT\tWORKDIR")
	for _, g := range gs {
		owner := "-"
		if g.OwnerConn != 0 {
			owner = fmt.Sprintf("conn %d pid %d", g.OwnerConn, g.OwnerPID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%v\t%d\t%s\t%s\t%s\n",
			g.Name, g.Provider, g.Model, g.Owned, owner, g.Alive, g.Pending, g.Purpose, g.Parent, g.WorkDir)
	}
	return w.Flush()
}

func tail() error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Time{})
	if err := c.WriteRequest(&broker.Request{ID: "tail", Type: broker.TypeTail, Tail: &broker.TailRequest{}}); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			return err
		}
		switch resp.Type {
		case broker.TypeTailing:
			continue
		case broker.TypeEvent:
			if err := enc.Encode(resp.Event); err != nil {
				return err
			}
		case broker.TypeError:
			return resp.Error.Err()
		}
	}
}

func usageCmd(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	refresh := fs.Bool("refresh", false, "fetch before answering")
	asJSON := fs.Bool("json", false, "print the snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := roundTrip(&broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{Refresh: *refresh}})
	if err != nil {
		return err
	}
	u := resp.Usage
	if *asJSON {
		os.Stdout.Write(u.Backends)
		fmt.Println()
		return nil
	}
	var backends []claudia.PlanUsage
	if err := json.Unmarshal(u.Backends, &backends); err != nil {
		return err
	}
	if u.FetchedAt.IsZero() {
		fmt.Printf("not fetched yet")
		if u.Error != "" {
			fmt.Printf(": %s", u.Error)
		}
		fmt.Println()
		return nil
	}
	fmt.Printf("fetched %s ago\n", time.Since(u.FetchedAt).Round(time.Second))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tSTATUS\tBAND\tWINDOWS")
	for _, b := range backends {
		band := claudia.ClassifyPlan(b, time.Now(), nil)
		var wins []string
		for _, win := range b.Windows {
			s := string(win.Name)
			if win.RemainingPercent != nil {
				s += fmt.Sprintf("=%.0f%%", *win.RemainingPercent)
			}
			wins = append(wins, s)
		}
		st := string(b.Status)
		if b.Reason != "" && b.Status != claudia.PlanUsageAvailable {
			st += " (" + clip(b.Reason, reasonWidth) + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.Provider, st, band.Weekly, strings.Join(wins, " "))
	}
	return w.Flush()
}

// reasonWidth bounds an unavailable reason in the usage table; the full
// text is one --json away.
const reasonWidth = 56

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func release(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	detach := fs.Bool("detach", false, "drop ownership but keep the seat running")
	force := fs.Bool("force", false, "operator exit: detach a grant another connection owns (implies --detach); the seat keeps running and the old owner is told")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("release: exactly one grant name")
	}
	disp := broker.DispositionStop
	if *detach || *force {
		disp = broker.DispositionDetach
	}
	_, err := roundTrip(&broker.Request{Type: broker.TypeRelease, Release: &broker.ReleaseRequest{Name: fs.Arg(0), Disposition: disp, Force: *force}})
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", fs.Arg(0), disp)
	return nil
}

// launchd user agent (macOS). brew services will carry the same plist once
// the formula ships (🎯T2.7); this is the owner-installed form.
const launchdLabel = "com.marcelocantos.claudia-broker"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	bin := fs.String("bin", "", "claudia binary to run (default: this executable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exe := *bin
	if exe == "" {
		p, err := os.Executable()
		if err != nil {
			return err
		}
		exe, _ = filepath.EvalSymlinks(p)
	}
	stateDir, err := broker.StateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(stateDir, "broker.log")
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>broker</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
</dict>
</plist>
`, launchdLabel, exe, logPath, logPath, launchdEnvXML())
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, launchdLabel)
	_ = exec.Command("launchctl", "bootout", target).Run()
	// bootout returns before the service is gone; a bootstrap that lands
	// while the label still exists fails with an opaque I/O error. Wait
	// for the old instance to unload.
	for i := 0; i < 50; i++ {
		if err := exec.Command("launchctl", "print", target).Run(); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed %s\n  plist  %s\n  binary %s\n  log    %s\n", launchdLabel, path, exe, logPath)
	return nil
}

// launchdEnv is the environment the service runs with, captured from the
// installing shell. launchd gives a user agent PATH=/usr/bin:/bin and
// little else: provider binaries would not resolve, and a TUI started in
// tmux without TERM / LANG / SHELL does not paint the same frames the
// owner's shell produces (the Claude Session live smoke failed exactly
// that way on 2026-09-12 with PATH and HOME alone). Only these names are
// carried; secrets in the shell stay in the shell.
var launchdEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
	"TERM", "COLORTERM", "LANG", "LC_ALL", "LC_CTYPE",
	"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"CLAUDE_BIN", "CODEX_BIN", "GROK_BIN", "CURSOR_BIN", "GROK_HOME",
	"CLAUDIA_GROK_CONNECT",
	"CLAUDIA_PLAN_CACHE", "CLAUDIA_CODEX_AUTH_PATH", "CLAUDIA_MODEL_INTEL",
}

// launchdEnvXML renders the captured environment as plist dict entries.
func launchdEnvXML() string {
	var b strings.Builder
	for _, k := range launchdEnv {
		v := os.Getenv(k)
		if v == "" {
			if k == "PATH" {
				v = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
			} else {
				continue
			}
		}
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", k, xmlEscape(v))
	}
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func uninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	uid := os.Getuid()
	if out, err := exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, launchdLabel)).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "launchctl bootout: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("uninstalled %s\n", launchdLabel)
	return nil
}
