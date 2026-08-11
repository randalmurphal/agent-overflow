package sessionimport

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/gitroot"
	"agent-overflow/internal/store"
)

// projectIndex answers "which AO project does this session's cwd belong to"
// without running git.
//
// A session's cwd is where the PROVIDER ran, which is routinely a linked
// worktree — AO parks its own under `<configDir>/worktrees/<project>/<branch>`,
// nowhere near the repository. Matching that path against project roots alone
// groups every worktree session under a project of its own, named after the
// branch. So the cwd is first resolved to its MAIN repository root
// (`gitroot.MainRoot`, git's `--git-common-dir` semantics) — pure filesystem
// reads, no subprocess.
//
// A cwd whose worktree has been DELETED cannot be resolved that way at all:
// there is nothing left to walk. Those are covered from the repository side,
// by the worktree registrations each known project still holds
// (`gitroot.RegisteredWorktrees`, read once per project per scan). A
// registration is folded into byPath under the project that owns it, so the
// ordinary probes place a dead worktree with no special case of their own.
type projectIndex struct {
	// byPath maps a path to the project covering it: every known project's own
	// root, plus every worktree path those projects have registered.
	byPath map[string]store.Project
	// roots is byPath's key set, longest first — the containment probe's
	// iteration order. The projects themselves are read back out of byPath;
	// a second path-keyed map would be the same map under another name.
	roots []string
	// resolved memoizes the WHOLE answer for one distinct cwd, for one scan.
	resolved map[string]resolvedWorkspace
}

// resolvedWorkspace is everything a row's decoration needs about one session
// cwd, and it is memoized as a unit because every field costs filesystem work:
// a real home holds ~1600 rows over ~120 distinct cwds, so answering per row
// is a stat and a repository walk apiece for the same handful of answers.
type resolvedWorkspace struct {
	// projectID is the AO project the cwd's sessions are stamped with, empty
	// when no known project covers it.
	projectID string
	// label is that project's name — or, when there is none, the name the
	// import will give the project it creates, so the row reads the same
	// before and after.
	label string
	// known is whether AO already has a project row covering the cwd.
	known bool
	// missing is whether the cwd itself is gone. Importing still works;
	// resuming the session there will not.
	missing bool
}

func newProjectIndex(ctx context.Context, s *store.Store) (*projectIndex, error) {
	projects, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	index := &projectIndex{
		byPath:   make(map[string]store.Project, len(projects)),
		roots:    make([]string, 0, len(projects)),
		resolved: make(map[string]resolvedWorkspace),
	}
	for _, project := range projects {
		index.cover(filepath.Clean(project.Path), project)
	}
	// Registrations are folded in only once EVERY real project row is placed.
	// A project row sitting on a worktree path is one the user has been using,
	// and another repository's registration of that path must not displace it.
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Clean(project.Path)
		registered, err := gitroot.RegisteredWorktrees(path)
		if err != nil {
			// A registry that cannot be read costs only the DELETED-worktree
			// grouping for this project: a live worktree still resolves off
			// the filesystem, and a dead one falls back to the ungrouped row
			// plus its "workspace no longer exists" warning — the behaviour
			// that shipped before the registry existed. Failing the whole
			// listing for it would be out of proportion, so it is logged
			// rather than returned.
			log.Printf("session import: worktree registrations of %s are unreadable, so sessions from its deleted worktrees stay ungrouped: %v", path, err)
			continue
		}
		for _, worktree := range registered {
			index.cover(worktree, project)
		}
	}
	// Longest first so a nested project wins over the repo containing it.
	sortLongestFirst(index.roots)
	return index, nil
}

// cover records path as belonging to project. First writer wins, which decides
// two different collisions the same way: a real project row always precedes
// the registration pass, and among registrations a worktree belongs to exactly
// one repository — two projects claiming it means one of them is a stale row.
func (p *projectIndex) cover(path string, project store.Project) {
	if _, taken := p.byPath[path]; taken {
		return
	}
	p.byPath[path] = project
	p.roots = append(p.roots, path)
}

func sortLongestFirst(paths []string) {
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
}

// stamp applies one cwd's resolution to a row.
func (p *projectIndex) stamp(row *Row) {
	resolved := p.resolve(filepath.Clean(row.ProjectPath))
	row.ProjectID = resolved.projectID
	row.ProjectLabel = resolved.label
	row.KnownProject = resolved.known
	if resolved.missing {
		row.Warnings = append(row.Warnings, fmt.Sprintf(
			"The workspace %s no longer exists. Importing still works; resuming the session will not.",
			row.ProjectPath))
	}
}

// resolve answers everything about one distinct session cwd, once per scan.
func (p *projectIndex) resolve(workspace string) resolvedWorkspace {
	if cached, ok := p.resolved[workspace]; ok {
		return cached
	}

	// One stat decides two things: the row's missing-workspace warning, and
	// whether walking up from the cwd is worth attempting — MainRoot refuses a
	// path that does not exist, deliberately, so a dead worktree is answered
	// from the registrations already folded into byPath.
	_, statErr := os.Stat(workspace)
	answer := resolvedWorkspace{missing: statErr != nil}

	root := workspace
	if !answer.missing {
		if main, ok := gitroot.MainRoot(workspace); ok {
			root = main
		}
	}

	// Most specific first. A project row AT the cwd wins outright: it may sit
	// on a worktree path (one an earlier build of this scan created, or one
	// the user opened directly), and resolving to the repository would move
	// those sessions out of the project they have been living in. Only then
	// does the repository root answer, and only then the cwd's own containment
	// — which is what places a directory inside a deleted worktree.
	project, known := p.byPath[workspace]
	if !known {
		project, known = p.covering(root)
	}
	if !known && root != workspace {
		project, known = p.covering(workspace)
	}

	if known {
		answer.projectID = project.ID
		answer.label = project.Name
		answer.known = true
	} else {
		// No project yet, so the label names the REPOSITORY rather than the
		// worktree directory (which is a branch name). A cwd with no known
		// project can carry no registration either — registrations only exist
		// for projects AO already has — so root is the honest answer here.
		answer.label = filepath.Base(root)
	}

	p.resolved[workspace] = answer
	return answer
}

// covering returns the project at path, or the nearest one whose root contains
// it. roots is sorted longest first, so the most specific project wins.
func (p *projectIndex) covering(path string) (store.Project, bool) {
	if project, ok := p.byPath[path]; ok {
		return project, true
	}
	for _, root := range p.roots {
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return p.byPath[root], true
		}
	}
	return store.Project{}, false
}
