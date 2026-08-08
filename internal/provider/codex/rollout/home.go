package rollout

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrOutsideCodexHome means a rollout path does not live under the Codex home
// this reader was pointed at.
//
// `threads.rollout_path` is an absolute path stored in a SQLite file AO does
// not own. Codex writes it, but nothing stops another process — or a corrupt
// row, or a database copied in from elsewhere — from naming `/etc/shadow` or
// `~/.ssh/id_ed25519`. Every path AO stats, opens, or reports back to the
// frontend from that column therefore has to be proven to be inside the home
// first: without the check, listing alone leaks the existence and size of
// arbitrary files, and importing reads their contents into a thread.
var ErrOutsideCodexHome = errors.New("rollout: path is outside the Codex home")

// PathInHome resolves rolloutPath and proves it sits under codexHome,
// returning the cleaned absolute path callers should use from then on.
//
// The check is lexical on cleaned absolute paths, which is what makes it
// usable BEFORE the file is touched — the point is to never stat or open a
// path outside the home, not to diagnose one afterwards. A symlink INSIDE the
// home that points out of it is deliberately still followed: Codex's own
// sessions directory is a legitimate symlink target on relocated homes, and
// refusing those would break real users to defend against an attacker who
// already has write access to the Codex home.
//
// A home that has MOVED (a rollout path recorded against `~/.codex` on
// another machine, or before a relocation) is the same answer: not in this
// home, so not readable. Callers surface it per row as a skip with a reason,
// never as a failure of the whole listing.
func PathInHome(codexHome, rolloutPath string) (string, error) {
	home := strings.TrimSpace(codexHome)
	if home == "" {
		return "", fmt.Errorf("rollout: no Codex home to resolve %q against: %w", rolloutPath, ErrOutsideCodexHome)
	}
	target := strings.TrimSpace(rolloutPath)
	if target == "" {
		return "", fmt.Errorf("rollout: empty rollout path: %w", ErrOutsideCodexHome)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("rollout: resolve Codex home %s: %w", home, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("rollout: resolve rollout path %s: %w", target, err)
	}
	rel, err := filepath.Rel(absHome, absTarget)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("rollout: %s is not inside %s: %w", absTarget, absHome, ErrOutsideCodexHome)
	}
	return absTarget, nil
}
