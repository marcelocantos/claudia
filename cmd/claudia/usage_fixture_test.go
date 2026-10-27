package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func fixtureTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/private/tmp", "claudia-fx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
func fixtureBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claudia")
	cmd := exec.Command("go", "build", "-o", path, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	return path
}
func fixtureServe(t *testing.T, binary, dir string) func() {
	t.Helper()
	cmd := exec.Command(binary, "broker", "serve", "--state-dir", dir, "--test-usage-fixture", "--no-resume")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	drained := make(chan struct{})
	var output strings.Builder
	go func() {
		defer close(drained)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			line := scan.Text()
			output.WriteString(line + "\n")
			if strings.Contains(line, "usage fixture ready") {
				select {
				case <-ready:
				default:
					close(ready)
				}
			}
		}
	}()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-exited
			<-drained
		}
	}
	t.Cleanup(stop)
	select {
	case <-ready:
		return stop
	case <-exited:
		<-drained
		t.Fatalf("fixture exited before readiness: %s", output.String())
	case <-t.Context().Done():
		stop()
		t.Fatalf("fixture readiness interrupted: %s", output.String())
	}
	return nil
}
func fixturePost(t *testing.T, dir string, readings []claudia.PlanUsage, token string) int {
	t.Helper()

	body, err := json.Marshal(struct {
		Backends []claudia.PlanUsage `json:"backends"`
	}{readings})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, fixtureSocket))
	}}}
	req, _ := http.NewRequest("POST", "http://fixture/usage", bytes.NewReader(body))
	req.Header.Set("X-Fixture-Capability", token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
func fixturePush(t *testing.T, dir string, readings []claudia.PlanUsage) {
	t.Helper()
	secret, err := os.ReadFile(filepath.Join(dir, fixtureCapability))
	if err != nil {
		t.Fatal(err)
	}
	if code := fixturePost(t, dir, readings, string(secret)); code != 200 {
		t.Fatalf("fixture push status %d", code)
	}
}

// fixtureReadReplica reads only the independently linked consumer database.
// No test writes broker or consumer SQL directly.
func fixtureReadReplica(t *testing.T, r *controlplane.Replica) map[claudia.Provider]claudia.PlanUsage {
	t.Helper()
	result := map[claudia.Provider]claudia.PlanUsage{}
	err := r.Read(func(db *sqlpipe.Database) error {
		q, err := db.Query("SELECT provider, status, success_at, reading_json FROM claudia_plan_usage")
		if err != nil {
			return err
		}
		for _, row := range q.Rows {
			provider, status, success, body := fmt.Sprint(row[0]), fmt.Sprint(row[1]), fmt.Sprint(row[2]), fmt.Sprint(row[3])
			if status != string(claudia.PlanUsageAvailable) {
				continue
			}
			var usage claudia.PlanUsage
			if err := json.Unmarshal([]byte(body), &usage); err != nil {
				return err
			}
			if success == "" || usage.FetchedAt.IsZero() || usage.Provider != claudia.Provider(provider) {
				return fmt.Errorf("invalid published row for %s", provider)
			}
			result[usage.Provider] = usage
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func fixtureAwaitBand(t *testing.T, r *controlplane.Replica, provider claudia.Provider, band claudia.PlanBand, remaining float64) claudia.PlanUsage {
	t.Helper()
	// The subscription returns an initial snapshot and subsequent replica
	// changesets. No host-speed-dependent poll deadline decides the verdict.
	updates, err := r.SubscribeQuery(t.Context(), "SELECT provider, status, success_at, reading_json FROM claudia_plan_usage")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				t.Fatal("replica subscription closed before desired generation")
			}
			if u, found := fixtureReadReplica(t, r)[provider]; found && len(u.Windows) > 0 && u.Windows[0].RemainingPercent != nil && *u.Windows[0].RemainingPercent == remaining {
				w := u.Windows[0]
				if w.Band != band || claudia.ClassifyPlan(u, u.FetchedAt, nil).Weekly != band {
					t.Fatalf("%s: broker band=%s consumer verdict=%s, want %s", provider, w.Band, claudia.ClassifyPlan(u, u.FetchedAt, nil).Weekly, band)
				}
				return u
			}
		case <-t.Context().Done():
			t.Fatalf("replica did not deliver %s %s/%v: %v", provider, band, remaining, t.Context().Err())
		}
	}
}
func fixtureWeekly(provider claudia.Provider, remaining, used float64, reset time.Time) claudia.PlanUsage {
	return claudia.PlanUsage{Provider: provider, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset, LimitWindow: 7 * 24 * time.Hour}}}
}
func fixtureLinkedReplica(t *testing.T, dir string) (*controlplane.Replica, context.CancelFunc) {
	t.Helper()
	replica, err := controlplane.OpenReplica(filepath.Join(t.TempDir(), "consumer.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = replica.Link(ctx, filepath.Join(dir, "broker.sock.control")) }()
	t.Cleanup(func() { cancel(); _ = replica.Close() })
	return replica, cancel
}

// J20's four transitions travel real CLI -> broker owner transaction -> sqlpipe
// replication -> consumer database. Classification is asserted on typed rows.
func TestT228CLIUsageFixtureJ20(t *testing.T) {
	dir, binary := fixtureTestDir(t), fixtureBinary(t)
	fixtureServe(t, binary, dir)
	replica, _ := fixtureLinkedReplica(t, dir)
	now := time.Now().UTC()
	aheadReset := now.Add(7 * 24 * time.Hour / 2)
	healthyReset := now.Add(7 * 24 * time.Hour * 9 / 10)
	initial := fixtureWeekly(claudia.ProviderGrok, 30, 70, aheadReset)
	if code := fixturePost(t, dir, []claudia.PlanUsage{initial}, "invalid"); code != 403 {
		t.Fatalf("unauthorized update: %d", code)
	}
	fixturePush(t, dir, []claudia.PlanUsage{initial})
	fixtureAwaitBand(t, replica, claudia.ProviderGrok, claudia.PlanBandAhead, 30)
	fixturePush(t, dir, []claudia.PlanUsage{fixtureWeekly(claudia.ProviderGrok, 90, 10, healthyReset)})
	fixtureAwaitBand(t, replica, claudia.ProviderGrok, claudia.PlanBandOK, 90)
	providers := []claudia.Provider{claudia.ProviderGrok, claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderCursor}
	all := make([]claudia.PlanUsage, 0, len(providers))
	for _, p := range providers {
		all = append(all, fixtureWeekly(p, 0, 100, healthyReset))
	}
	fixturePush(t, dir, all)
	for _, p := range providers {
		u := fixtureAwaitBand(t, replica, p, claudia.PlanBandExhausted, 0)
		if claudia.HasAvailableTokens(u, u.FetchedAt, nil) {
			t.Fatalf("%s exhausted but eligible", p)
		}
	}
	secret, err := os.ReadFile(filepath.Join(dir, fixtureCapability))
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]claudia.PlanUsage(nil), all...)
	bad = append(bad, claudia.PlanUsage{Provider: claudia.Provider("invalid"), Status: claudia.PlanUsageAvailable})
	if code := fixturePost(t, dir, bad, string(secret)); code != 400 {
		t.Fatalf("invalid batch accepted: %d", code)
	}
	for _, p := range providers {
		fixtureAwaitBand(t, replica, p, claudia.PlanBandExhausted, 0)
	}
	all = all[:0]
	for _, p := range providers {
		all = append(all, fixtureWeekly(p, 90, 10, healthyReset))
	}
	fixturePush(t, dir, all)
	for _, p := range providers {
		fixtureAwaitBand(t, replica, p, claudia.PlanBandOK, 90)
	}
}

// J34's provider state is pinned on both sides of a simulated seat-start
// boundary. This does not launch a Jevons seat or assert migration; those
// system assertions belong to 🎯jevons/T1234.
func TestT228CLIUsageFixtureJ34ConsumerReconnect(t *testing.T) {
	dir, binary := fixtureTestDir(t), fixtureBinary(t)
	stop := fixtureServe(t, binary, dir)
	replica, cancel := fixtureLinkedReplica(t, dir)
	reset := time.Now().UTC().Add(7 * 24 * time.Hour * 9 / 10)
	before := []claudia.PlanUsage{fixtureWeekly(claudia.ProviderClaude, 90, 10, reset), fixtureWeekly(claudia.ProviderGrok, 90, 10, reset)}
	fixturePush(t, dir, before)
	fixtureAwaitBand(t, replica, claudia.ProviderClaude, claudia.PlanBandOK, 90)
	fixtureAwaitBand(t, replica, claudia.ProviderGrok, claudia.PlanBandOK, 90)
	// Simulated post-seat boundary: no actual Jevons seat is started here.
	fixturePush(t, dir, []claudia.PlanUsage{fixtureWeekly(claudia.ProviderClaude, 0, 100, reset), fixtureWeekly(claudia.ProviderGrok, 90, 10, reset)})
	fixtureAwaitBand(t, replica, claudia.ProviderClaude, claudia.PlanBandExhausted, 0)
	fixtureAwaitBand(t, replica, claudia.ProviderGrok, claudia.PlanBandOK, 90)
	cancel() // Disconnect the first consumer; leave broker running.
	reconnected, _ := fixtureLinkedReplica(t, dir)
	fixtureAwaitBand(t, reconnected, claudia.ProviderClaude, claudia.PlanBandExhausted, 0)
	fixtureAwaitBand(t, reconnected, claudia.ProviderGrok, claudia.PlanBandOK, 90)
	// Additional broker persistence coverage, distinct from J34 consumer bounce.
	stop()
	fixtureServe(t, binary, dir)
	restarted, _ := fixtureLinkedReplica(t, dir)
	fixtureAwaitBand(t, restarted, claudia.ProviderClaude, claudia.PlanBandExhausted, 0)
	fixtureAwaitBand(t, restarted, claudia.ProviderGrok, claudia.PlanBandOK, 90)
}
func TestT228FixtureRefusesUnsafeState(t *testing.T) {
	dir := fixtureTestDir(t)
	binary := fixtureBinary(t)
	defaultDir, err := broker.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(defaultDir, 0700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.Mkdir(bad, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", defaultDir, bad, filepath.Join(dir, "link")} {
		if path == filepath.Join(dir, "link") {
			if err := os.Symlink(dir, path); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(binary, "broker", "serve", "--test-usage-fixture", "--state-dir", path)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("accepted unsafe path %q: %s", path, out)
		}
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("default state wrote log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bad, "logs")); !os.IsNotExist(err) {
		t.Fatalf("unsafe state wrote log: %v", err)
	}
}
