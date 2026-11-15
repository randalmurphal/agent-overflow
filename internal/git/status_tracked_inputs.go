package git

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// churnInputs is what a status scan reads for its churn before any churn
// diff runs: the memo the last scan left, the working tree's root and
// index, and, beside the status read, the effective config and a
// whole-tree diff.
type churnInputs struct {
	// start is a time before status ran.
	start time.Time
	prev  trackedChurnMemo
	root  worktreeRootProof
	index indexSnapshot

	// done closes when config and early are set.
	done chan struct{}
	// config is read when the scan can key its results, before early runs.
	config gitConfig
	// early is nil when the last scan left results to choose paths by, or
	// when HEAD named no commit or the diff failed.
	early *earlyChurnDiff
}

// startChurnInputs reads cwd's churn inputs at start and starts reading
// the rest beside the status read. A scan with no stored results to
// choose paths by diffs the whole tree then, so it costs the longer of
// the status read and the diff, not their sum.
func (c *Core) startChurnInputs(cwd string, start time.Time) *churnInputs {
	prev, _ := c.trackedChurnMemos.snapshot(cwd)
	in := &churnInputs{start: start, prev: prev, done: make(chan struct{})}
	in.root = c.worktreeRoot(cwd, prev.root)
	in.index = snapshotIndex(in.root.indexPath, start)
	unnamed := c.attributeFilesUnnamed.Load()
	keyable := in.root.isRoot && !unnamed
	needsEarly := !prev.warm || unnamed
	go func() {
		defer close(in.done)
		if keyable {
			in.config = c.effectiveConfig(cwd)
		}
		if needsEarly {
			in.early = c.runEarlyChurnDiff(cwd)
		}
	}()
	return in
}

// wait blocks until config and early are set.
func (in *churnInputs) wait() {
	<-in.done
}

// gitConfig is git's effective config for a cwd, includes resolved, as
// `git config --list -z` prints it.
type gitConfig struct {
	list string
	// read is false when git did not list it.
	read bool
}

// effectiveConfig reads git's effective config for cwd.
func (c *Core) effectiveConfig(cwd string) gitConfig {
	config, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, args: []string{"config", "--list", "-z"}})
	if err != nil || config.exitCode != 0 {
		return gitConfig{}
	}
	return gitConfig{list: config.stdout, read: true}
}

// earlyChurnDiff is a successful whole-tree churn diff run beside the
// status read, pinned to the commit HEAD named before it ran.
type earlyChurnDiff struct {
	commit string
	result commandResult
}

// runEarlyChurnDiff diffs the whole working tree against the commit HEAD
// names now. It returns nil when HEAD names no commit or the diff fails;
// the scan then runs its own diff and reports that one's failure.
func (c *Core) runEarlyChurnDiff(cwd string) *earlyChurnDiff {
	head, err := c.run(cwd, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || head.exitCode != 0 {
		return nil
	}
	commit := strings.TrimSpace(head.stdout)
	result, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, maxBytes: statusOutputLimit, args: churnDiffArgs(commit, nil)})
	if err != nil || result.exitCode != 0 {
		return nil
	}
	return &earlyChurnDiff{commit: commit, result: result}
}

// usableFor reports whether the diff ran against commit.
func (d *earlyChurnDiff) usableFor(commit string) bool {
	return d != nil && d.commit == commit
}

// indexSnapshot is the index file's stat as a scan began. Status and the
// churn diff each read the index, and what a path's diff prints depends
// on whether the index lists it. A scan stores its results only when both
// read the same index: the file had been left alone since before the scan
// began, and is the same file after the diff.
type indexSnapshot struct {
	// path is empty when git did not name the index.
	path  string
	stat  worktreeStat
	state worktreeFileState
}

// snapshotIndex reads the stat of the index file at path.
func snapshotIndex(path string, scanStart time.Time) indexSnapshot {
	if path == "" {
		return indexSnapshot{}
	}
	stat, state := statWorktreeFile(path, scanStart, true)
	return indexSnapshot{path: path, stat: stat, state: state}
}

// unchanged reports whether the index is still the file the scan began
// with. A repository with no index file has the same one while it has
// none.
func (s indexSnapshot) unchanged(scanStart time.Time) bool {
	if s.path == "" || s.state == worktreeFileUnsettled {
		return false
	}
	stat, state := statWorktreeFile(s.path, scanStart, true)
	return state == s.state && stat == s.stat
}

// worktreeRootProof is git's answer to whether a cwd is its working
// tree's root and where that tree's index is, and the cwd's .git entry
// the answers were given under.
type worktreeRootProof struct {
	gitEntry fs.FileInfo
	isRoot   bool
	// indexPath is empty when cwd is not the root.
	indexPath string
}

// worktreeRoot reports whether cwd is the root of its working tree, and
// that tree's index. A .git entry in cwd does not settle it: git skips
// one that is not a repository and uses the one above. Git is asked again
// only when the .git entry is not the one prev was answered under.
func (c *Core) worktreeRoot(cwd string, prev worktreeRootProof) worktreeRootProof {
	gitEntry, err := os.Lstat(filepath.Join(cwd, ".git"))
	if err != nil {
		return worktreeRootProof{}
	}
	if prev.gitEntry != nil && os.SameFile(prev.gitEntry, gitEntry) {
		return prev
	}
	paths, found, err := c.revParsePaths(context.Background(), cwd, 2, "--show-toplevel", "--git-path", "index")
	if err != nil || !found {
		return worktreeRootProof{}
	}
	rootInfo, rootErr := os.Stat(paths[0])
	cwdInfo, cwdErr := os.Stat(cwd)
	proof := worktreeRootProof{gitEntry: gitEntry, isRoot: rootErr == nil && cwdErr == nil && os.SameFile(rootInfo, cwdInfo)}
	if proof.isRoot {
		proof.indexPath = paths[1]
	}
	return proof
}
