// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Watcher observes the Claude Code projects directory for new JSONL
// files and feeds observations into a [State]. Each per-workdir
// subdirectory of the projects root gets its own fsnotify watch added
// lazily — either when Start sees an existing subdirectory or when a
// CREATE event on a new one arrives.
type Watcher struct {
	state *State
	fsw   *fsnotify.Watcher

	mu       sync.Mutex
	watched  map[string]bool

	done      chan struct{}
	closeOnce sync.Once
}

// NewWatcher constructs a Watcher bound to the given State.
func NewWatcher(state *State) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		state:   state,
		fsw:     fsw,
		watched: make(map[string]bool),
		done:    make(chan struct{}),
	}, nil
}

// Start ensures the projects root exists, watches it, seeds existing
// subdirectories, and spawns the event goroutine. Non-blocking.
func (w *Watcher) Start() error {
	root := w.state.projectsDir
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := w.fsw.Add(root); err != nil {
		return err
	}
	w.mu.Lock()
	w.watched[root] = true
	w.mu.Unlock()

	// Seed existing per-workdir subdirs. Rollovers that happen after
	// startup in a cwd that was already being watched by Claude Code
	// get picked up this way.
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				w.addProjectDir(filepath.Join(root, e.Name()))
			}
		}
	}

	go w.run()
	return nil
}

// Close stops the event goroutine and releases the fsnotify watcher.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		_ = w.fsw.Close()
	})
}

func (w *Watcher) run() {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("daemon watcher error", "err", err)
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	if ev.Op&fsnotify.Create == 0 {
		return
	}
	root := w.state.projectsDir
	// A new per-workdir subdir under the root: start watching it.
	if filepath.Dir(ev.Name) == root {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			w.addProjectDir(ev.Name)
			return
		}
	}
	// A new .jsonl under a watched project subdir: emit an observation.
	if strings.HasSuffix(ev.Name, ".jsonl") {
		w.emit(ev.Name)
	}
}

func (w *Watcher) addProjectDir(dir string) {
	w.mu.Lock()
	already := w.watched[dir]
	if !already {
		w.watched[dir] = true
	}
	w.mu.Unlock()
	if already {
		return
	}
	if err := w.fsw.Add(dir); err != nil {
		slog.Warn("daemon watcher add failed", "dir", dir, "err", err)
		return
	}
	// Replay any jsonl files already present — they may be the first
	// sid of a session we're about to hear about via Register.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				w.emit(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// emit translates a jsonl path into an ObserveEscaped call on the
// state. The escaped cwd is the basename of the parent directory;
// the sid is the filename minus the .jsonl extension.
func (w *Watcher) emit(path string) {
	parent := filepath.Base(filepath.Dir(path))
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if sid == "" {
		return
	}
	w.state.ObserveEscaped(parent, sid)
}
