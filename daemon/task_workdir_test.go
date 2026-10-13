package daemon

import (
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// TestTaskRunRejectsRelativeWorkDir: a relative or empty WorkDir must be
// refused, never silently run in the daemon's own cwd.
func TestTaskRunRejectsRelativeWorkDir(t *testing.T) {
	for _, wd := range []string{"", ".", "sub/dir"} {
		t.Run("wd="+wd, func(t *testing.T) {
			f := newFixture(t)
			f.boot(t, nil)
			conn, err := broker.Dial(f.sock)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			raw, err := claudia.EncodeTaskConfigWire(claudia.TaskConfig{Provider: claudia.ProviderGrok, WorkDir: wd})
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteRequest(&broker.Request{ID: "r", Type: broker.TypeTaskRun,
				TaskRun: &broker.TaskRunRequest{Task: raw, Prompt: "x"}}); err != nil {
				t.Fatal(err)
			}
			resp, err := conn.ReadResponse()
			if err != nil {
				t.Fatal(err)
			}
			if resp.Type != broker.TypeError || resp.Error == nil || resp.Error.Code != broker.CodeMalformed || resp.Error.Field != "task.workdir" {
				t.Fatalf("relative workdir %q = type %s error %+v", wd, resp.Type, resp.Error)
			}
		})
	}
}
