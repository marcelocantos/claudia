package claudia

import (
	"fmt"
	"path/filepath"
)

// ResolveClientWorkDir turns a caller-relative workdir ("" or ".", or any
// relative path) into an absolute, symlink-resolved path in the caller's own
// cwd. The broker daemon runs elsewhere (its own cwd is the claudia checkout
// under supervisord), so a relative WorkDir must never cross the wire.
// Symlinks are resolved when the path exists, matching startWithBackendContext.
func ResolveClientWorkDir(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("workdir %q: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}
