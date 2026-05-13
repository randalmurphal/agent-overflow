package git

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RandomStashSuffix returns a short hex token suitable for tagging
// `git stash push -m <prefix>-<suffix>` messages so concurrent
// carry-over operations on the same repo never collide on the
// stash-list lookup. Falls back to a unix-nano hex if the system RNG
// is unavailable — the goal is uniqueness, not unpredictability.
func RandomStashSuffix() string {
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(token[:])
}

// StashPushIncludeUntracked stashes the working tree (staged + unstaged +
// untracked) under message. Returns created=false when git reports nothing
// to stash; the caller skips the carry-over in that case.
func (c *Core) StashPushIncludeUntracked(cwd, message string) (bool, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return false, fmt.Errorf("git stash message is required")
	}
	stdout, _, err := c.Execute(cwd, "stash", "push", "-u", "-m", message)
	if err != nil {
		return false, err
	}
	if strings.Contains(stdout, "No local changes to save") {
		return false, nil
	}
	return true, nil
}

// findStashRefByMessage scans the stash list for the entry whose message ends
// with the supplied marker and returns its ref (e.g. "stash@{0}"). Looking up
// by message means concurrent carry-overs in the same repo never collide.
//
// Match is HasSuffix only — `git stash push -m <message>` produces a
// reflog subject of `On <branch>: <message>`, so the marker always
// sits at the end. A Contains fallback would let an unrelated stash
// whose message happens to contain our hex token (e.g. an external
// `git stash push -m "ao-carry-deadbeef wip"` from outside the app)
// resolve preferentially over the intended ref.
func (c *Core) findStashRefByMessage(cwd, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("git stash message is required")
	}
	stdout, _, err := c.Execute(cwd, "stash", "list", "--format=%gd %s")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, " ")
		if idx <= 0 {
			continue
		}
		ref := line[:idx]
		desc := strings.TrimSpace(line[idx+1:])
		if strings.HasSuffix(desc, message) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("stash entry %q not found", message)
}

// StashApplyByMessage looks up the stash entry by message and applies it.
// `apply` (not `pop`) leaves the stash entry intact so a failed apply doesn't
// destroy the snapshot.
func (c *Core) StashApplyByMessage(cwd, message string) error {
	ref, err := c.findStashRefByMessage(cwd, message)
	if err != nil {
		return err
	}
	_, stderr, err := c.Execute(cwd, "stash", "apply", ref)
	if err != nil {
		trimmed := strings.TrimSpace(stderr)
		if trimmed == "" {
			trimmed = err.Error()
		}
		return fmt.Errorf("git stash apply %s: %s", ref, trimmed)
	}
	return nil
}

// StashDropByMessage drops the stash entry whose message matches.
func (c *Core) StashDropByMessage(cwd, message string) error {
	ref, err := c.findStashRefByMessage(cwd, message)
	if err != nil {
		return err
	}
	_, _, err = c.Execute(cwd, "stash", "drop", ref)
	return err
}
