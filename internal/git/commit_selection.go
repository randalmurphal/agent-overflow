package git

import (
	"errors"
	"fmt"
	"strings"
)

// pathArgBytes bounds the path bytes one argv carries, well under every
// supported platform's argument limit.
const pathArgBytes = 32 << 10

// CommitSelection names what a commit covers: every uncommitted change, or
// the working-tree state of exactly the listed paths. One of the two is set,
// so a caller that lost its paths gets an error, not a commit of everything.
type CommitSelection struct {
	// All covers every uncommitted change, untracked files included.
	All bool
	// Paths are matched literally. A rename takes both of its paths.
	Paths []string
}

// Validate reports a selection that names nothing, names both forms, or
// holds a path git cannot take.
func (s CommitSelection) Validate() error {
	if s.All {
		if len(s.Paths) > 0 {
			return errors.New("commit selection: paths given with all")
		}
		return nil
	}
	if len(s.Paths) == 0 {
		return errors.New("commit selection: no files selected")
	}
	for _, p := range s.Paths {
		if p == "" || strings.ContainsRune(p, 0) {
			return fmt.Errorf("commit selection: invalid path %q", p)
		}
	}
	return nil
}

// CommitSelected commits what the selection covers and returns the new
// commit SHA. All stages everything first (`git add -A`). Paths commits
// exactly those paths and leaves every other change as it is, including
// whatever the user staged.
//
// A Paths selection is refused while CommitsWholeOnly: git refuses it during
// a merge or cherry-pick, and during a revert it would record the paths and
// end the revert with the rest of it left staged.
func (c *Core) CommitSelected(cwd, subject, body string, selection CommitSelection) (string, error) {
	if strings.TrimSpace(subject) == "" {
		return "", fmt.Errorf("git commit subject is required")
	}
	if err := selection.Validate(); err != nil {
		return "", err
	}
	if selection.All {
		if err := c.StageAll(cwd); err != nil {
			return "", err
		}
		return c.Commit(cwd, subject, body)
	}
	if op := c.pendingOperation(cwd); pendingOperationCommitsWhole(op) {
		return "", fmt.Errorf("git commit: a %s is in progress and records every change; selected files cannot be committed on their own", op)
	}
	return c.commitPaths(cwd, subject, body, selection.Paths)
}

func (c *Core) commitPaths(cwd, subject, body string, paths []string) (string, error) {
	// `commit --only` matches paths against the index and HEAD, so a file
	// git has never seen is recorded as intent-to-add first.
	untracked, err := c.untrackedAmong(cwd, paths)
	if err != nil {
		return "", err
	}
	if len(untracked) > 0 {
		if err := c.runPathspecCommand(cwd, untracked, "add", "--intent-to-add"); err != nil {
			return "", err
		}
	}
	args := append([]string{"commit", "--only"}, commitArgs(subject, body)[1:]...)
	if err := c.runPathspecCommand(cwd, paths, args...); err != nil {
		if undoErr := c.dropIndexEntries(cwd, untracked); undoErr != nil {
			err = errors.Join(err, fmt.Errorf("leaving new files marked intent-to-add: %w", undoErr))
		}
		return "", err
	}

	stdout, _, err := c.Execute(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// dropIndexEntries removes the index entries of the given untracked paths,
// which is all an intent-to-add left there. Unlike `git reset` it consults
// nothing else, so a deletion the user staged for the same path stays staged.
func (c *Core) dropIndexEntries(cwd string, untracked []string) error {
	if len(untracked) == 0 {
		return nil
	}
	var stdin strings.Builder
	for _, p := range untracked {
		// An untracked nested repository is listed as a directory.
		stdin.WriteString(strings.TrimSuffix(p, "/"))
		stdin.WriteByte(0)
	}
	_, _, err := c.executeSpec(commandSpec{
		binary: "git",
		cwd:    cwd,
		stdin:  stdin.String(),
		args:   []string{"update-index", "-z", "--force-remove", "--stdin"},
	})
	return err
}

// runPathspecCommand runs a git subcommand over paths matched literally,
// handed over on stdin so their count is not bounded by argv.
func (c *Core) runPathspecCommand(cwd string, paths []string, args ...string) error {
	argv := append([]string{"--literal-pathspecs"}, args...)
	argv = append(argv, "--pathspec-from-file=-", "--pathspec-file-nul")
	_, _, err := c.executeSpec(commandSpec{
		binary: "git",
		cwd:    cwd,
		stdin:  strings.Join(paths, "\x00") + "\x00",
		args:   argv,
	})
	return err
}

// untrackedAmong returns the given paths that are untracked and not ignored.
func (c *Core) untrackedAmong(cwd string, paths []string) ([]string, error) {
	var untracked []string
	for start := 0; start < len(paths); {
		end, size := start, 0
		for end < len(paths) && (end == start || size+len(paths[end]) <= pathArgBytes) {
			size += len(paths[end])
			end++
		}
		args := append([]string{"--literal-pathspecs", "ls-files", "-z", "--others", "--exclude-standard", "--"}, paths[start:end]...)
		stdout, _, err := c.Execute(cwd, args...)
		if err != nil {
			return nil, err
		}
		for p := range strings.SplitSeq(stdout, "\x00") {
			if p != "" {
				untracked = append(untracked, p)
			}
		}
		start = end
	}
	return untracked, nil
}
