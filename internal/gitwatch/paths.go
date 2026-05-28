package gitwatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// canonicalize resolves p to an absolute, symlink-free path AND
// returns the absolute (pre-symlink) form so callers can refuse based
// on either. macOS /tmp goes through symlinks (/tmp → /private/tmp,
// /etc → /private/etc); without keeping both forms we'd miss a
// user-supplied "/etc" because canonicalize resolves it to "/private/etc"
// which isn't equal to "/etc" any more.
//
// Intentionally distinct from gitops.CanonicalPath: that helper does
// best-effort resolution and falls back to the original on error,
// because non-existent paths are a normal case for branch comparison /
// diff display. Subscribe-time MUST surface errors instead so a bad
// cwd doesn't quietly install a watcher rooted at the literal user
// input.
func canonicalize(p string) (abs, canon string, err error) {
	abs, err = filepath.Abs(p)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	return abs, filepath.Clean(resolved), nil
}

// systemPaths are absolute paths (and the canonical form of paths that
// symlink elsewhere) the manager refuses to watch. The list is
// intentionally small — defense-in-depth against a workspace path that
// resolves to a system root, not a general allow-list. CreateThread is
// LocalOnly so the wire input is already trusted; this is a backstop
// for a misconfigured row, a developer-time mistake, or a future bug
// in path resolution.
var systemPaths = map[string]struct{}{
	"/":            {},
	"/etc":         {},
	"/var":         {},
	"/usr":         {},
	"/bin":         {},
	"/sbin":        {},
	"/opt":         {},
	"/private":     {},
	"/private/etc": {},
	"/private/var": {},
	"/System":      {},
	"/Library":     {},
	"/Volumes":     {},
	"/Users":       {},
	"/home":        {},
	"/proc":        {},
	"/sys":         {},
	"/dev":         {},
	`C:\`:          {},
	`C:\Windows`:   {},
	`C:\Users`:     {},
}

// rejectSystemPath returns an error if either the user-supplied
// absolute path OR its symlink-resolved canonical form is a system
// root a recursive fs watcher should never touch. Also refuses
// non-directories so a path that resolves to a device node or socket
// can't slip through.
func rejectSystemPath(abs, canon string) error {
	if _, blocked := systemPaths[abs]; blocked {
		return fmt.Errorf("gitwatch: refusing to watch system path %q", abs)
	}
	if _, blocked := systemPaths[canon]; blocked {
		return fmt.Errorf("gitwatch: refusing to watch system path %q", canon)
	}
	info, err := os.Stat(canon)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("gitwatch: cwd is not a directory")
	}
	return nil
}
