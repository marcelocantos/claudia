// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"strings"
	"testing"
)

// 🎯T47.10: TaskConfig.SessionID is the provider-neutral resume handle and
// TaskConfig.ClaudeID is its deprecated alias. Either spelling reaches the
// backend as req.SessionID; SessionID wins when both are set.
func TestT4710SessionIDAndClaudeIDAreOneHandle(t *testing.T) {
	cases := []struct {
		name string
		cfg  TaskConfig
		want string
	}{
		{"SessionID only", TaskConfig{SessionID: "sid-new"}, "sid-new"},
		{"ClaudeID only", TaskConfig{ClaudeID: "sid-old"}, "sid-old"},
		{"both set, SessionID wins", TaskConfig{SessionID: "sid-new", ClaudeID: "sid-old"}, "sid-new"},
		{"neither", TaskConfig{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := newTaskWithBackend(tc.cfg, nil)
			if got := task.SessionID(); got != tc.want {
				t.Fatalf("Task.SessionID() = %q, want %q", got, tc.want)
			}
			if got := task.ClaudeID(); got != tc.want {
				t.Fatalf("Task.ClaudeID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The daemon protocol carries the handle under both keys so a daemon built
// before SessionID existed still resumes from claude_id, and decoding either
// key alone yields the same task.
func TestT4710TaskConfigWireCarriesBothSessionKeys(t *testing.T) {
	raw, err := EncodeTaskConfigWire(TaskConfig{ID: "t", ClaudeID: "sid-old"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if keys["session_id"] != "sid-old" || keys["claude_id"] != "sid-old" {
		t.Fatalf("wire = %s, want session_id and claude_id both %q", raw, "sid-old")
	}
	for _, in := range []string{
		`{"id":"t","session_id":"sid"}`,
		`{"id":"t","claude_id":"sid"}`,
		`{"id":"t","session_id":"sid","claude_id":"stale"}`,
	} {
		cfg, err := DecodeTaskConfigWire(json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := newTaskWithBackend(cfg, nil).SessionID(); got != "sid" {
			t.Fatalf("%s: resumed session %q, want %q", in, got, "sid")
		}
	}
}

// The acceptance for 🎯T47.10 is that `go doc TaskConfig` leads with a
// provider-neutral name. This reads the same source go doc reads: the
// struct's own comment names SessionID before ClaudeID, SessionID is
// declared before ClaudeID, and ClaudeID (field and method) carries a
// Deprecated paragraph pointing at SessionID.
func TestT4710GoDocTaskConfigLeadsWithSessionID(t *testing.T) {
	fset := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fset, "task.go", nil, goparser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var fieldDoc = map[string]string{}
	var fieldOrder []string
	var typeDoc string
	var methodDoc = map[string]string{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *goast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*goast.TypeSpec)
				if !ok || ts.Name.Name != "TaskConfig" {
					continue
				}
				typeDoc = d.Doc.Text()
				for _, fld := range ts.Type.(*goast.StructType).Fields.List {
					for _, n := range fld.Names {
						fieldOrder = append(fieldOrder, n.Name)
						fieldDoc[n.Name] = fld.Doc.Text()
					}
				}
			}
		case *goast.FuncDecl:
			if d.Recv != nil && (d.Name.Name == "SessionID" || d.Name.Name == "ClaudeID") {
				methodDoc[d.Name.Name] = d.Doc.Text()
			}
		}
	}
	if typeDoc == "" {
		t.Fatal("TaskConfig has no doc comment")
	}
	i, j := strings.Index(typeDoc, "SessionID"), strings.Index(typeDoc, "ClaudeID")
	if i < 0 || (j >= 0 && j < i) {
		t.Fatalf("TaskConfig doc must name SessionID before ClaudeID:\n%s", typeDoc)
	}
	si, ci := indexOf(fieldOrder, "SessionID"), indexOf(fieldOrder, "ClaudeID")
	if si < 0 || ci < 0 || si > ci {
		t.Fatalf("field order %v: SessionID must be declared before ClaudeID", fieldOrder)
	}
	for what, doc := range map[string]string{"field ClaudeID": fieldDoc["ClaudeID"], "method ClaudeID": methodDoc["ClaudeID"]} {
		if !strings.Contains(doc, "\nDeprecated: use SessionID") {
			t.Fatalf("%s must carry a Deprecated paragraph pointing at SessionID:\n%s", what, doc)
		}
	}
	if strings.Contains(fieldDoc["SessionID"], "Deprecated") || strings.Contains(methodDoc["SessionID"], "Deprecated") {
		t.Fatal("SessionID is the live name and must not be marked deprecated")
	}
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}
