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

// The fake forge's origin repositories: bare repositories under
// <data root>/forge-origin that the harness creates (HarnessForgeOrigin)
// and the e2e helpers push a pull's head to (e2e/tests/forge-helpers.ts,
// publishPullRequest), which the workspace's git proxy serves as the
// forge's origin. A commit the fake makes on a pull whose head
// repository is one of them (an applied suggestion) is written there as a
// real commit on the head branch and the pull's head ref, so the app can
// fetch the head the forge reports.
//
// The harness writes only to a repository it created in this process:
// the path, symlinks evaluated, must be the one it registered, inside the
// origin root, a bare repository by git's own answers, with no
// alternates, and git runs with an environment nothing outside can
// redirect. Anything else is refused and the fake keeps its generated
// sha. A component replaced between the check and the write is not
// guarded against.

// forgeOriginRoot is where the harness keeps the fake forge's origin
// repositories.
func forgeOriginRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "forge-origin")
}

// forgePullHeadRef is the ref a forge keeps a pull's head under.
func forgePullHeadRef(forge string, number int) string {
	if forge == "gitlab" {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// forgeOrigins are the origin repositories under root that this harness
// created, by canonical path.
type forgeOrigins struct {
	root string

	mu sync.Mutex
	// canonical is root with symlinks evaluated, set when the first
	// origin is created.
	canonical string
	created   map[string]bool
}

func newForgeOrigins(root string) *forgeOrigins {
	return &forgeOrigins{root: root, created: map[string]bool{}}
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
// as a forge's default branch, and registers it, answering its path. Creating one it made again answers the same path;
// any other directory already at the path, or a segment that is a
// symbolic link or a file, is refused.
func (o *forgeOrigins) create(project string) (string, error) {
	segments, err := projectSegments(project)
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.MkdirAll(o.root, 0o755); err != nil {
		return "", fmt.Errorf("create the forge origin root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(o.root)
	if err != nil {
		return "", fmt.Errorf("resolve the forge origin root: %w", err)
	}
	if o.canonical == "" {
		o.canonical = canonical
	} else if canonical != o.canonical {
		return "", fmt.Errorf("the forge origin root %s now resolves to %s, not %s", o.root, canonical, o.canonical)
	}
	dir, existed := canonical, false
	for _, s := range segments {
		dir = filepath.Join(dir, s)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", fmt.Errorf("create forge origin %s: %w", project, err)
			}
			existed = false
		case err != nil:
			return "", fmt.Errorf("create forge origin %s: %w", project, err)
		case !info.IsDir():
			return "", fmt.Errorf("forge origin %s: %s is not a directory", project, dir)
		default:
			existed = true
		}
	}
	if existed {
		if o.created[dir] {
			return dir, nil
		}
		return "", fmt.Errorf("forge origin %s: %s exists and is not one this harness created", project, dir)
	}
	if _, err := forgeOriginGit(dir, canonical, "", "", "init", "--bare", "--quiet", "--initial-branch=main"); err != nil {
		return "", err
	}
	o.created[dir] = true
	return dir, nil
}

// errNoForgeOrigin is a pull whose head repository has no origin
// directory: it is bound to none, which is no refusal.
var errNoForgeOrigin = errors.New("no origin repository")

// resolve answers the canonical path of project's origin when it is one
// create made and still is: the path with symlinks evaluated is the one
// registered and inside the root, it has no alternates, and git answers
// that it is that bare repository.
func (o *forgeOrigins) resolve(project string) (dir, root string, err error) {
	segments, err := projectSegments(project)
	if err != nil {
		return "", "", err
	}
	o.mu.Lock()
	root = o.canonical
	o.mu.Unlock()
	path := filepath.Join(append([]string{o.root}, segments...)...)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return "", "", errNoForgeOrigin
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", path, err)
	}
	o.mu.Lock()
	registered := o.created[real]
	o.mu.Unlock()
	if root == "" || !registered {
		return "", "", fmt.Errorf("%s resolves to %s, which this harness did not create", path, real)
	}
	if want := filepath.Join(append([]string{root}, segments...)...); real != want {
		return "", "", fmt.Errorf("%s resolves to %s, not %s", path, real, want)
	}
	if rel, err := filepath.Rel(root, real); err != nil || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("%s is outside the forge origin root %s", real, root)
	}
	if info, err := os.Lstat(real); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%s is not the directory this harness created", real)
	}
	for _, name := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(real, "objects", "info", name)); !errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%s has objects/info/%s", real, name)
		}
	}
	for _, check := range []struct{ args []string }{
		{[]string{"rev-parse", "--is-bare-repository"}},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}},
		{[]string{"rev-parse", "--absolute-git-dir"}},
	} {
		out, err := forgeOriginGit(real, root, "", "", check.args...)
		if err != nil {
			return "", "", err
		}
		want := real
		if check.args[1] == "--is-bare-repository" {
			want = "true"
		}
		if out != want {
			return "", "", fmt.Errorf("git %s in %s answered %q, want %q", strings.Join(check.args, " "), real, out, want)
		}
	}
	return real, root, nil
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
