package git

import (
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// churnContext keys what the churn diff reads besides the contents it
// compares, as git names it:
//   - the effective config, includes resolved (`git config --list`);
//   - the .gitattributes files in the directories above the changed
//     paths, and the index entry of each one status lists (git reads the
//     index's copy where the working tree has none);
//   - the repository's info/attributes and the attribute files outside
//     the repository (attributeSources), through any link, as git reads
//     them.
//
// A config that reads attributes from a tree (attr.tree, GIT_ATTR_SOURCE)
// is not keyed: the ref it names moves without the config changing.
//
// The commit is not part of it: a key names the blob the commit holds at
// its path, so a commit of other paths leaves a stored result valid.
//
// It returns false when cwd is not the working tree's root (status paths
// are relative to it) or one of these cannot be identified. Everything is
// read before the diff it keys: the config by early when that diff ran
// first, and each file by a stat that has not changed since scanStart.
func (c *Core) churnContext(cwd string, status porcelainStatus, scanStart time.Time, early *earlyChurnDiff) ([sha256.Size]byte, bool) {
	var none [sha256.Size]byte
	if c.attributeFilesUnnamed.Load() || !c.worktreeRoot(cwd, scanStart).isRoot {
		return none, false
	}
	commonDir, err := c.CommonDir(cwd)
	if err != nil {
		return none, false
	}
	config, ok := early.effectiveConfig()
	if !ok {
		config, ok = c.effectiveConfig(cwd)
	}
	if !ok || configNames(config, "attr.tree") || os.Getenv("GIT_ATTR_SOURCE") != "" {
		return none, false
	}
	configKey := sha256.Sum256([]byte(config))
	sources, ok := c.attributeSourcesUnder(cwd, configKey, scanStart)
	if !ok {
		return none, false
	}

	h := sha256.New()
	field := func(value string) {
		_, _ = io.WriteString(h, value)
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write(configKey[:])
	keyed := true

	// A directory's attribute file is read for every path under it. Only
	// the files present are keyed, so a change in a directory that has
	// none leaves the context as it was.
	type attributeFile struct {
		dir  string
		stat worktreeStat
	}
	var present []attributeFile
	seen := make(map[string]struct{})
	above := func(path string) {
		for dir := path; dir != ""; {
			dir = dir[:max(strings.LastIndexByte(dir, '/'), 0)]
			if _, visited := seen[dir]; visited {
				return
			}
			seen[dir] = struct{}{}
			// Git does not read an in-tree attribute file through a link.
			stat, state := statWorktreeFile(filepath.Join(cwd, filepath.FromSlash(dir), ".gitattributes"), scanStart, false)
			switch state {
			case worktreeFileSettled:
				present = append(present, attributeFile{dir: dir, stat: stat})
			case worktreeFileUnsettled:
				keyed = false
			}
		}
	}
	for i := range status.entries {
		entry := &status.entries[i]
		above(entry.path)
		if entry.origPath != "" {
			above(entry.origPath)
		}
		if isAttributesPath(entry.path) {
			field(entry.record)
			field(entry.origPath)
		}
	}
	// The order directories are met in follows the changed paths.
	slices.SortFunc(present, func(a, b attributeFile) int { return strings.Compare(a.dir, b.dir) })
	for _, file := range present {
		field(file.dir)
		file.stat.writeTo(h)
	}

	outside := []string{filepath.Join(commonDir, "info", "attributes"), sources.global, sources.system}
	for _, path := range outside {
		field(path)
		if path != "" && hashWorktreeFile(h, path, scanStart, true) == worktreeFileUnsettled {
			keyed = false
		}
	}
	return [sha256.Size]byte(h.Sum(nil)), keyed
}

func isAttributesPath(path string) bool {
	return path == ".gitattributes" || strings.HasSuffix(path, "/.gitattributes")
}

// effectiveConfig returns git's effective config for cwd, includes
// resolved, and false when git did not list it.
func (c *Core) effectiveConfig(cwd string) (string, bool) {
	config, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, args: []string{"config", "--list", "-z"}})
	if err != nil || config.exitCode != 0 {
		return "", false
	}
	return config.stdout, true
}

// configNames reports whether `git config --list -z` output sets key.
func configNames(stdout string, key string) bool {
	for record := range strings.SplitSeq(stdout, "\x00") {
		if name, _, _ := strings.Cut(record, "\n"); name == key {
			return true
		}
	}
	return false
}

// attributeSources are the attribute files git reads from outside the
// repository under one effective config, empty where git reads none.
type attributeSources struct {
	config         [sha256.Size]byte
	global, system string
}

// attributeSourcesUnder asks git where its global and system attribute
// files are, once per effective config: core.attributesFile moves the
// first, and the second belongs to the installation. It returns false
// when git did not say.
func (c *Core) attributeSourcesUnder(cwd string, config [sha256.Size]byte, now time.Time) (attributeSources, bool) {
	sources, ok := c.attributeSources.snapshot(cwd)
	if !ok || sources.config != config {
		global, globalKnown := c.gitVarPath(cwd, "GIT_ATTR_GLOBAL")
		if !globalKnown {
			return attributeSources{}, false
		}
		system, systemKnown := c.gitVarPath(cwd, "GIT_ATTR_SYSTEM")
		if !systemKnown {
			return attributeSources{}, false
		}
		sources = attributeSources{config: config, global: strings.Clone(global), system: strings.Clone(system)}
	}
	c.attributeSources.store(cwd, sources, now)
	return sources, true
}

// gitVarUsageExit is how git exits when it does not know a variable.
const gitVarUsageExit = 129

// gitVarPath returns the path `git var` prints for variable, empty when
// git has no such path, and false when git did not answer. A git that
// does not know the variable (before 2.42) never will, so nothing it
// diffs can be keyed: that is recorded for the process
// (attributeFilesUnnamed), and scans stop asking.
func (c *Core) gitVarPath(cwd string, variable string) (string, bool) {
	result, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, args: []string{"var", variable}})
	if err != nil {
		return "", false
	}
	switch {
	case result.exitCode == 0:
		return strings.TrimSuffix(result.stdout, "\n"), true
	case result.exitCode == 1 && result.stdout == "" && strings.TrimSpace(result.stderr) == "":
		return "", true // the variable is known and names nothing here
	case result.exitCode == gitVarUsageExit:
		c.attributeFilesUnnamed.Store(true)
		return "", false
	default:
		return "", false
	}
}

// worktreeRootProof is git's answer to whether a cwd is its working
// tree's root and where that tree's index is, and the cwd's .git entry
// the answers were given under.
type worktreeRootProof struct {
	gitEntry fs.FileInfo
	isRoot   bool
	// indexPath is empty when cwd is not the root or git named no index.
	indexPath string
}

// worktreeRoot reports whether cwd is the root of its working tree, and
// that tree's index. A .git entry in cwd does not settle it: git skips
// one that is not a repository and uses the one above. Git is asked once
// per .git entry.
func (c *Core) worktreeRoot(cwd string, now time.Time) worktreeRootProof {
	gitEntry, err := os.Lstat(filepath.Join(cwd, ".git"))
	if err != nil {
		return worktreeRootProof{}
	}
	proof, ok := c.worktreeRootProofs.snapshot(cwd)
	if !ok || !os.SameFile(proof.gitEntry, gitEntry) {
		root, found, err := c.revParsePath(cwd, "--show-toplevel")
		if err != nil || !found {
			return worktreeRootProof{}
		}
		rootInfo, rootErr := os.Stat(root)
		cwdInfo, cwdErr := os.Stat(cwd)
		proof = worktreeRootProof{gitEntry: gitEntry, isRoot: rootErr == nil && cwdErr == nil && os.SameFile(rootInfo, cwdInfo)}
		if proof.isRoot {
			proof.indexPath = c.indexPath(cwd)
		}
	}
	c.worktreeRootProofs.store(cwd, proof, now)
	return proof
}

// indexPath returns the index file of cwd's working tree, and "" when
// git did not name it.
func (c *Core) indexPath(cwd string) string {
	result, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, args: []string{"rev-parse", "--git-path", "index"}})
	if err != nil || result.exitCode != 0 {
		return ""
	}
	path := strings.TrimSuffix(result.stdout, "\n")
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return path
}
