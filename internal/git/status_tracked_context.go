package git

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// churnContext keys what the churn diff reads for every path besides the
// contents it compares, as git names it:
//   - the effective config, includes resolved (in.config);
//   - the repository's info/attributes and the attribute files outside
//     the repository (attributeSources), through any link, as git reads
//     them.
//
// The in-tree attribute files a path reads are keyed per path
// (attributeChains). A config that reads attributes from a tree
// (attr.tree, GIT_ATTR_SOURCE) is not keyed: the ref it names moves
// without the config changing.
//
// The commit is not part of it: a key names the blob the commit holds at
// its path, so a commit of other paths leaves a stored result valid.
//
// It returns the attribute sources it keyed by, and false when cwd is not
// the working tree's root (status paths are relative to it) or one of
// these cannot be identified. The config was read before any diff ran,
// and each file is keyed by a stat that has not changed since in.start.
func (c *Core) churnContext(cwd string, in *churnInputs) ([sha256.Size]byte, attributeSources, bool) {
	var none [sha256.Size]byte
	if c.attributeFilesUnnamed.Load() || !in.root.isRoot || !in.config.read {
		return none, in.prev.sources, false
	}
	if configNames(in.config.list, "attr.tree") || os.Getenv("GIT_ATTR_SOURCE") != "" {
		return none, in.prev.sources, false
	}
	commonDir, err := c.CommonDir(cwd)
	if err != nil {
		return none, in.prev.sources, false
	}
	configKey := sha256.Sum256([]byte(in.config.list))
	sources, ok := c.attributeSourcesUnder(cwd, configKey, in.prev.sources)
	if !ok {
		return none, in.prev.sources, false
	}

	h := sha256.New()
	_, _ = h.Write(configKey[:])
	keyed := true
	for _, path := range []string{filepath.Join(commonDir, "info", "attributes"), sources.global, sources.system} {
		_, _ = io.WriteString(h, path)
		_, _ = h.Write([]byte{0})
		if path != "" && hashWorktreeFile(h, path, in.start, true) == worktreeFileUnsettled {
			keyed = false
		}
	}
	return [sha256.Size]byte(h.Sum(nil)), sources, keyed
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

// attributeChains keys the in-tree attribute files git reads for a path:
// the .gitattributes of each directory from the working tree's root down
// to the path's own, and the status record of each that status lists (git
// reads the index's copy where the working tree has none). A change to
// one rekeys only the paths under its directory. Each directory is keyed
// once per scan.
type attributeChains struct {
	cwd       string
	scanStart time.Time
	// listed holds the records of the attribute files status lists, by
	// directory.
	listed map[string][]string
	dirs   map[string]attributeChain
}

// attributeChain is the key of the attribute files one directory's paths
// read. keyed is false when one of them was not left alone since the
// scan began.
type attributeChain struct {
	key   [sha256.Size]byte
	keyed bool
}

func newAttributeChains(cwd string, entries []statusEntry, scanStart time.Time) *attributeChains {
	chains := &attributeChains{cwd: cwd, scanStart: scanStart, listed: make(map[string][]string), dirs: make(map[string]attributeChain)}
	for i := range entries {
		if path := entries[i].path; path == ".gitattributes" || strings.HasSuffix(path, "/.gitattributes") {
			dir := parentDir(path)
			chains.listed[dir] = append(chains.listed[dir], entries[i].record)
		}
	}
	return chains
}

// of returns the chain of the attribute files git reads for path.
func (a *attributeChains) of(path string) attributeChain {
	return a.dir(parentDir(path))
}

func (a *attributeChains) dir(dir string) attributeChain {
	if chain, ok := a.dirs[dir]; ok {
		return chain
	}
	h := sha256.New()
	keyed := true
	if dir != "" {
		parent := a.dir(parentDir(dir))
		_, _ = h.Write(parent.key[:])
		keyed = parent.keyed
	}
	// Git does not read an in-tree attribute file through a link.
	if hashWorktreeFile(h, filepath.Join(a.cwd, filepath.FromSlash(dir), ".gitattributes"), a.scanStart, false) == worktreeFileUnsettled {
		keyed = false
	}
	for _, record := range a.listed[dir] {
		_, _ = io.WriteString(h, record)
		_, _ = h.Write([]byte{0})
	}
	chain := attributeChain{key: [sha256.Size]byte(h.Sum(nil)), keyed: keyed}
	a.dirs[dir] = chain
	return chain
}

// parentDir returns the directory of a slash-separated path relative to
// the working tree's root, "" for the root.
func parentDir(path string) string {
	return path[:max(strings.LastIndexByte(path, '/'), 0)]
}

// attributeSources are the attribute files git reads from outside the
// repository under one effective config, empty where git reads none.
type attributeSources struct {
	config         [sha256.Size]byte
	global, system string
}

// attributeSourcesUnder returns where git's global and system attribute
// files are under config, asking git only when prev was answered under
// another config: core.attributesFile moves the first, and the second
// belongs to the installation. It returns false when git did not say.
func (c *Core) attributeSourcesUnder(cwd string, config [sha256.Size]byte, prev attributeSources) (attributeSources, bool) {
	if prev.config == config {
		return prev, true
	}
	global, globalKnown := c.gitVarPath(cwd, "GIT_ATTR_GLOBAL")
	if !globalKnown {
		return attributeSources{}, false
	}
	system, systemKnown := c.gitVarPath(cwd, "GIT_ATTR_SYSTEM")
	if !systemKnown {
		return attributeSources{}, false
	}
	return attributeSources{config: config, global: global, system: system}, true
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
