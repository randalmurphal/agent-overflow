package harnessrpc

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"agent-overflow/internal/harness/forgefake"
)

// The fake forge's origin repositories: bare repositories the harness
// creates (HarnessForgeOrigin) and the e2e helpers push a pull's head to
// (e2e/tests/forge-helpers.ts, publishPullRequest), which the
// workspace's git proxy serves as the forge's origin. A commit the fake
// makes on a pull whose head repository is one of them (an applied
// suggestion) is written there as a real commit on the head branch and
// the pull's head ref, so the app can fetch the head the forge reports.
//
// Two rules keep that write inside the test's own repositories:
//
//   - Every directory from the origin root down to an origin is one this
//     process created by exclusive creation: the root is a fresh
//     directory under the canonical data root the boot validated, made
//     on the first HarnessForgeOrigin, and nothing found on disk is ever
//     adopted as a root or an origin.
//   - Immediately before a write, the origin's path and its whole tree
//     are plain (forgeOriginPlain): no symbolic link and nothing but
//     directories and regular files at any depth.
//
// Beside them, git must answer that the origin is a bare repository whose
// common directory is itself, it has no alternates, and git runs with no
// inherited GIT_ variable. Anything else is refused and the fake keeps
// its generated sha.

// forgeOriginRootPattern names the origin root under the data root, one
// per harness process.
const forgeOriginRootPattern = "forge-origin-*"

// forgeOriginMaxEntries bounds the walk of one origin before a write. A
// test origin is a few pushed branches: tens of entries, a few hundred
// with loose objects. One past it is refused.
const forgeOriginMaxEntries = 10_000

// forgePullHeadRef is the ref a forge keeps a pull's head under.
func forgePullHeadRef(forge string, number int) string {
	if forge == "gitlab" {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// forgeOrigins are the origin repositories this harness created.
type forgeOrigins struct {
	// dataRoot is the canonical data root the boot validated.
	dataRoot string

	mu sync.Mutex
	// root is the origin root this process created, "" until the first
	// origin.
	root string
	// dirs are the directories under root this process created, origins
	// and their parents; origins the origin directories among them.
	dirs    map[string]bool
	origins map[string]bool
}

func newForgeOrigins(dataRoot string) *forgeOrigins {
	return &forgeOrigins{dataRoot: dataRoot, dirs: map[string]bool{}, origins: map[string]bool{}}
}

// ForgeOrigin is a created origin: the origin root, which the e2e git
// proxy serves, and the repository's path under it.
type ForgeOrigin struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

// projectSegments splits a forge project path ("grp/sub/tool") into its
// directory segments under the root, the last named <name>.git.
func projectSegments(project string) ([]string, error) {
	segments := strings.Split(project, "/")
	for _, s := range segments {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `\:`) || strings.HasPrefix(s, "-") {
			return nil, fmt.Errorf("forge project %q is not a path of plain segments", project)
		}
	}
	segments[len(segments)-1] += ".git"
	return segments, nil
}

// create makes the bare origin repository of project, its HEAD on main
// as a forge's default branch, and registers it. Every directory it
// makes is made by exclusive creation; one already on disk that this
// process did not make is refused. Creating an origin again answers the
// same one.
func (o *forgeOrigins) create(project string) (ForgeOrigin, error) {
	segments, err := projectSegments(project)
	if err != nil {
		return ForgeOrigin{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.root == "" {
		root, err := os.MkdirTemp(o.dataRoot, forgeOriginRootPattern)
		if err != nil {
			return ForgeOrigin{}, fmt.Errorf("create the forge origin root: %w", err)
		}
		o.root = root
	}
	dir := o.root
	for _, s := range segments {
		dir = filepath.Join(dir, s)
		if o.dirs[dir] {
			continue
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return ForgeOrigin{}, fmt.Errorf("create forge origin %s: %w", project, err)
		}
		o.dirs[dir] = true
	}
	if !o.origins[dir] {
		if _, err := forgeOriginGit(dir, o.root, "", "", "init", "--bare", "--quiet", "--initial-branch=main"); err != nil {
			return ForgeOrigin{}, err
		}
		o.origins[dir] = true
	}
	return ForgeOrigin{Root: o.root, Path: dir}, nil
}

// errNoForgeOrigin is a pull whose head repository has no origin: it is
// bound to none, which is no refusal.
var errNoForgeOrigin = errors.New("no origin repository")

// resolve answers project's origin when this process created it and it
// is still a plain bare repository (forgeOriginPlain, git's answers, no
// alternates).
func (o *forgeOrigins) resolve(project string) (dir, root string, err error) {
	segments, err := projectSegments(project)
	if err != nil {
		return "", "", err
	}
	o.mu.Lock()
	root = o.root
	if root != "" {
		dir = filepath.Join(append([]string{root}, segments...)...)
	}
	registered := dir != "" && o.origins[dir]
	o.mu.Unlock()
	if !registered {
		if dir == "" {
			return "", "", errNoForgeOrigin
		}
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return "", "", errNoForgeOrigin
		}
		return "", "", fmt.Errorf("%s is not an origin this harness created", dir)
	}
	if err := forgeOriginPlain(root, dir); err != nil {
		return "", "", err
	}
	for _, name := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(dir, "objects", "info", name)); !errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%s has objects/info/%s", dir, name)
		}
	}
	for _, check := range [][]string{
		{"rev-parse", "--is-bare-repository"},
		{"rev-parse", "--path-format=absolute", "--git-common-dir"},
	} {
		out, err := forgeOriginGit(dir, root, "", "", check...)
		if err != nil {
			return "", "", err
		}
		want := dir
		if check[1] == "--is-bare-repository" {
			want = "true"
		}
		if out != want {
			return "", "", fmt.Errorf("git %s in %s answered %q, want %q", strings.Join(check, " "), dir, out, want)
		}
	}
	return dir, root, nil
}

// forgeOriginPlain refuses dir unless every component from root down to
// it is a directory and no symbolic link, and every entry under it, at
// any depth, is a directory or a regular file and no symbolic link, at
// most forgeOriginMaxEntries of them. Read with Lstat, so no walked path
// resolves outside dir.
func forgeOriginPlain(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	path := root
	for _, s := range append([]string{"."}, strings.Split(rel, string(filepath.Separator))...) {
		path = filepath.Join(path, s)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsDir() {
			return fmt.Errorf("%s is not a plain directory (%s)", path, info.Mode().Type())
		}
	}
	entries := 0
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entries++; entries > forgeOriginMaxEntries {
			return fmt.Errorf("%s holds more than %d entries", dir, forgeOriginMaxEntries)
		}
		if t := d.Type(); t != 0 && t != fs.ModeDir {
			return fmt.Errorf("%s is not a plain file or directory (%s)", path, t)
		}
		return nil
	})
}

// commit is forgefake.Options.Commit for the origins: "" when the pull's
// head repository has no origin this harness created (a refusal is
// logged) or it lacks the parent commit, else the sha of the commit
// written: Path set to Content (keeping the path's mode) on top of
// Parent, with Message, on refs/heads/<Branch> (which must still point
// at Parent) and the pull's head ref.
func (o *forgeOrigins) commit(req forgefake.CommitRequest) (string, error) {
	if req.Project == "" {
		return "", nil
	}
	// The origin is checked here and written by the git runs below. A
	// component replaced in between is not guarded against: only the
	// test's own processes write under the origin root.
	bare, root, err := o.resolve(req.Project)
	if errors.Is(err, errNoForgeOrigin) {
		return "", nil
	}
	if err != nil {
		log.Printf("harness: forge origin of %s not written: %v", req.Project, err)
		return "", nil
	}
	index, err := os.CreateTemp("", "ao-forge-origin-index-*")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		return "", err
	}
	// git writes the index; only the name is needed.
	if err := os.Remove(indexPath); err != nil {
		return "", err
	}
	defer func() {
		if err := os.Remove(indexPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("harness: remove %s: %v", indexPath, err)
		}
	}()
	git := func(stdin string, args ...string) (string, error) {
		return forgeOriginGit(bare, root, indexPath, stdin, args...)
	}
	if _, err := git("", "cat-file", "-e", req.Parent+"^{commit}"); err != nil {
		return "", nil // a pull seeded with a head the repository lacks is bound to none
	}
	mode := "100644"
	entry, err := git("", "ls-tree", req.Parent, "--", req.Path)
	if err != nil {
		return "", err
	}
	if fields := strings.Fields(entry); len(fields) > 0 {
		mode = fields[0]
	}
	blob, err := git(req.Content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := git("", "read-tree", req.Parent); err != nil {
		return "", err
	}
	if _, err := git("", "update-index", "--add", "--cacheinfo", mode+","+blob+","+req.Path); err != nil {
		return "", err
	}
	tree, err := git("", "write-tree")
	if err != nil {
		return "", err
	}
	sha, err := git(req.Message, "commit-tree", tree, "-p", req.Parent, "-F", "-")
	if err != nil {
		return "", err
	}
	if req.Branch == "" {
		return "", errors.New("the pull has no head branch")
	}
	if _, err := git("", "update-ref", "refs/heads/"+req.Branch, sha, req.Parent); err != nil {
		return "", err
	}
	if _, err := git("", "update-ref", forgePullHeadRef(req.Forge, req.Number), sha); err != nil {
		return "", err
	}
	return sha, nil
}

// forgeOriginGit runs git on the bare repository dir from inside it, with
// forgeOriginGitEnv, and answers its trimmed output.
func forgeOriginGit(dir, root, index, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = forgeOriginGitEnv(dir, root, index)
	cmd.Stdin = strings.NewReader(stdin)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// forgeOriginGitEnv runs git on the bare repository dir and nothing else:
// every inherited GIT_ variable is dropped (GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_COMMON_DIR, GIT_CONFIG_*, ...),
// discovery stops at the origin root, replace refs are ignored, no system
// or global configuration is read, and the identity is fixed, as the
// forge commits. index is the index file, "" for none.
func forgeOriginGitEnv(dir, root, index string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_DIR="+dir,
		"GIT_CEILING_DIRECTORIES="+root,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Agent Overflow Harness",
		"GIT_AUTHOR_EMAIL=harness@agent-overflow.invalid",
		"GIT_COMMITTER_NAME=Agent Overflow Harness",
		"GIT_COMMITTER_EMAIL=harness@agent-overflow.invalid",
	)
	if index != "" {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	return env
}
