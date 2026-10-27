package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	capPath := filepath.Join(dir, fixtureCapability)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(capPath); err == nil {
			if _, err = os.Stat(filepath.Join(dir, fixtureSocket)); err == nil {
				return stop
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	t.Fatalf("fixture did not start: %s", output.String())
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
func fixtureRows(t *testing.T, r *controlplane.Replica) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := r.Read(func(db *sqlpipe.Database) error {
		q, err := db.Query("SELECT provider, status, reading_json FROM claudia_plan_usage")
		if err != nil {
			return err
		}
		for _, row := range q.Rows {
			result[fmt.Sprint(row[0])] = fmt.Sprint(row[1]) + " " + fmt.Sprint(row[2])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestT228CLIUsageFixtureReplicatesAndPersists(t *testing.T) {
	dir := fixtureTestDir(t)
	binary := fixtureBinary(t)
	stop := fixtureServe(t, binary, dir)
	replica, err := controlplane.OpenReplica(filepath.Join(t.TempDir(), "consumer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = replica.Link(ctx, filepath.Join(dir, "broker.sock.control")) }()
	pct := func(n float64) *float64 { return &n }
	readings := []claudia.PlanUsage{{Provider: claudia.ProviderClaude, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowSession, RemainingPercent: pct(0)}}}, {Provider: claudia.ProviderCodex, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: pct(67)}}}}
	if code := fixturePost(t, dir, readings, "wrong-capability"); code != 403 {
		t.Fatalf("unauthorized write: %d", code)
	}
	fixturePush(t, dir, readings)
	await := func(provider, match string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(fixtureRows(t, replica)[provider], match) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("replica did not see %s %s: %v", provider, match, fixtureRows(t, replica))
	}
	await("claude", "exhausted")
	await("codex", "67")
	secret, _ := os.ReadFile(filepath.Join(dir, fixtureCapability))
	bad := append([]claudia.PlanUsage(nil), readings...)
	bad = append(bad, claudia.PlanUsage{Provider: claudia.Provider("invalid"), Status: claudia.PlanUsageAvailable})
	if code := fixturePost(t, dir, bad, string(secret)); code != 400 {
		t.Fatalf("invalid batch accepted: %d", code)
	}
	await("claude", "exhausted")
	// J20 phases: under/ahead => healthy => every tracked plan exhausted => healthy.
	readings = []claudia.PlanUsage{}
	for _, provider := range []claudia.Provider{claudia.ProviderGrok, claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderCursor} {
		readings = append(readings, claudia.PlanUsage{Provider: provider, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: pct(0), UsedPercent: pct(100)}}})
	}
	fixturePush(t, dir, readings)
	for _, u := range readings {
		await(string(u.Provider), "exhausted")
	}
	for i := range readings {
		readings[i].Windows[0].RemainingPercent = pct(90)
		readings[i].Windows[0].UsedPercent = pct(10)
	}
	fixturePush(t, dir, readings)
	for _, u := range readings {
		await(string(u.Provider), `"remaining_percent":90`)
	}
	// J34's restart is the consumer (Jevons), not the broker. A new
	// consumer file links to the still-running broker and sees the last hot phase.
	readings[1].Windows[0].RemainingPercent = pct(0)
	readings[1].Windows[0].UsedPercent = pct(100)
	fixturePush(t, dir, readings)
	await("claude", "exhausted")
	other, err := controlplane.OpenReplica(filepath.Join(t.TempDir(), "reconnected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	go func() { _ = other.Link(otherCtx, filepath.Join(dir, "broker.sock.control")) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(fixtureRows(t, other)["claude"], "exhausted") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(fixtureRows(t, other)["claude"], "exhausted") {
		t.Fatal("reconnected consumer did not see retained hot Claude reading")
	}
	stop() // Broker restart is additional persistence coverage; J34 restarts consumer only.
	stop = fixtureServe(t, binary, dir)
	_ = stop
	cancel()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = replica.Link(ctx, filepath.Join(dir, "broker.sock.control")) }()
	readings[0].Windows[0].RemainingPercent = pct(55)
	fixturePush(t, dir, readings)
	await("grok", "55")
}
func TestT228FixtureRefusesUnsafeState(t *testing.T) {
	dir := fixtureTestDir(t)
	binary := fixtureBinary(t)
	bad := filepath.Join(dir, "bad")
	if err := os.Mkdir(bad, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", bad, filepath.Join(dir, "link")} {
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
	if _, err := os.Stat(filepath.Join(bad, "logs")); !os.IsNotExist(err) {
		t.Fatalf("unsafe state wrote log: %v", err)
	}
}
