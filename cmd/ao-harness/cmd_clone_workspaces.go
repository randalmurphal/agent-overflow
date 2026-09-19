package main

// Workspace relocation for `clone`. The copied store still names the
// developer's REAL repositories in projects.path and in the thread and
// work-item worktree columns. Booting that copy under the harness spawns a
// mock provider with cwd inside a real checkout, and a scenario writeFile
// step then writes into it. Every workspace path in the COPY is rewritten
// to a throwaway git fixture inside the clone's own root, and the fixtures
// are created so git-dependent features keep operating on real history.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness"
)

// cloneWorkspacesDirName mirrors internal/harnessrpc/seed.go: generated
// project workspaces live under <dataRoot>/workspaces.
const cloneWorkspacesDirName = "workspaces"

// cloneWorktreesDirName mirrors production: App.defaultWorktreePath places
// linked worktrees at <configDir>/worktrees/<base(projectPath)>/<segment>,
// and <configDir> is the app data dir inside the data root.
const cloneWorktreesDirName = "worktrees"

// workspaceRelocation is the relocation's receipt.
type workspaceRelocation struct {
	Projects      int           `json:"projects"`
	Worktrees     int           `json:"worktrees"`
	WorkspacesDir string        `json:"workspacesDir"`
	WorktreesDir  string        `json:"worktreesDir"`
	Tables        []scrubResult `json:"tables,omitempty"`
}

// projectFixture is one relocated project: the throwaway repo built for it
// plus the linked worktrees built under it.
type projectFixture struct {
	dirName    string
	dir        string
	worktrees  map[string]string // real worktree path -> fixture worktree path
	usedNames  map[string]struct{}
	usedBranch map[string]struct{}
}

type projectPathUpdate struct {
	id   string
	path string
}

type threadPathUpdate struct {
	id          string
	workspace   string
	worktree    string
	hasWorktree bool
}

// relocateClonedWorkspaces rewrites every workspace path in the copy and
// builds the git fixtures those paths now name.
func relocateClonedWorkspaces(e *env, targetDB, targetRoot, targetDir string, force bool) (workspaceRelocation, error) {
	out := workspaceRelocation{
		WorkspacesDir: filepath.Join(targetRoot, cloneWorkspacesDirName),
		WorktreesDir:  filepath.Join(targetDir, cloneWorktreesDirName),
	}
	if force {
		// A re-clone rebuilds the fixtures from the new database, so the
		// previous clone's repos and worktrees must go first. Only these two
		// generated trees, and only inside the target root.
		if err := removeCloneFixtureTree(targetRoot, out.WorkspacesDir); err != nil {
			return out, err
		}
		if err := removeCloneFixtureTree(targetRoot, out.WorktreesDir); err != nil {
			return out, err
		}
	}

	db, err := openClonedDatabase(targetDB)
	if err != nil {
		return out, err
	}
	defer db.Close()

	if err := requireColumns(db, "projects", "id", "path", "slug", "name"); err != nil {
		return out, err
	}
	if err := requireColumns(db, "threads", "id", "project_id", "workspace_path", "worktree_path"); err != nil {
		return out, err
	}

	fixtures, projectUpdates, usedDirNames, err := buildProjectFixtures(db, out.WorkspacesDir)
	if err != nil {
		return out, err
	}
	threadUpdates, byRealWorktree, worktrees, err := buildThreadFixtures(db, fixtures, usedDirNames, out.WorkspacesDir, out.WorktreesDir)
	if err != nil {
		return out, err
	}
	out.Projects = len(projectUpdates)
	out.Worktrees = worktrees

	if err := applyProjectPaths(db, projectUpdates); err != nil {
		return out, err
	}
	if err := applyThreadPaths(db, threadUpdates); err != nil {
		return out, err
	}
	for _, table := range []string{"work_items", "work_item_units"} {
		result, err := relocateWorkItemWorktrees(e, db, targetDB, table, byRealWorktree)
		if err != nil {
			return out, err
		}
		out.Tables = append(out.Tables, result)
	}
	return out, nil
}

// buildProjectFixtures creates one throwaway repo per project row and
// returns the fixture by project id plus the projects.path rewrites.
func buildProjectFixtures(db *sql.DB, workspacesDir string) (map[string]*projectFixture, []projectPathUpdate, map[string]struct{}, error) {
	rows, err := db.Query(`SELECT id, COALESCE(path,''), COALESCE(slug,''), COALESCE(name,'') FROM projects ORDER BY id`)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read projects: %w", err)
	}
	defer rows.Close()

	fixtures := make(map[string]*projectFixture)
	used := make(map[string]struct{})
	var updates []projectPathUpdate
	for rows.Next() {
		var id, path, slug, name string
		if err := rows.Scan(&id, &path, &slug, &name); err != nil {
			return nil, nil, nil, fmt.Errorf("scan project: %w", err)
		}
		fixture, err := createProjectFixture(workspacesDir, used, fixtureDirName(slug, name, path))
		if err != nil {
			return nil, nil, nil, err
		}
		fixtures[id] = fixture
		updates = append(updates, projectPathUpdate{id: id, path: fixture.dir})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("read projects: %w", err)
	}
	return fixtures, updates, used, nil
}

func createProjectFixture(workspacesDir string, used map[string]struct{}, base string) (*projectFixture, error) {
	dirName := uniqueName(used, base)
	dir := filepath.Join(workspacesDir, dirName)
	if err := harness.CreateRepo(dir, harness.RepoSpec{}); err != nil {
		return nil, fmt.Errorf("create workspace fixture %s: %w", dir, err)
	}
	return &projectFixture{
		dirName:    dirName,
		dir:        dir,
		worktrees:  make(map[string]string),
		usedNames:  make(map[string]struct{}),
		usedBranch: make(map[string]struct{}),
	}, nil
}

// buildThreadFixtures resolves every thread's workspace to a fixture,
// creating a linked worktree for each distinct real worktree path a
// project's threads name. Threads sharing one real worktree share one
// fixture worktree.
func buildThreadFixtures(db *sql.DB, fixtures map[string]*projectFixture, usedDirNames map[string]struct{}, workspacesDir, worktreesDir string) ([]threadPathUpdate, map[string]string, int, error) {
	rows, err := db.Query(`SELECT id, COALESCE(project_id,''), COALESCE(workspace_path,''), COALESCE(worktree_path,'')
	                         FROM threads ORDER BY id`)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read threads: %w", err)
	}
	defer rows.Close()

	byRealWorktree := make(map[string]string)
	// Orphan threads keep their own fixtures: threads.project_id is
	// nullable and its FK is not enforced on every connection, so a thread
	// can name a project row that is not there.
	unowned := make(map[string]*projectFixture)
	var updates []threadPathUpdate
	worktrees := 0
	for rows.Next() {
		var id, projectID, workspace, worktree string
		if err := rows.Scan(&id, &projectID, &workspace, &worktree); err != nil {
			return nil, nil, 0, fmt.Errorf("scan thread: %w", err)
		}
		fixture := fixtures[projectID]
		if fixture == nil {
			key := projectID
			if key == "" {
				key = "\x00workspace\x00" + workspace
			}
			if fixture = unowned[key]; fixture == nil {
				fixture, err = createProjectFixture(workspacesDir, usedDirNames, fmt.Sprintf("unowned-%d", len(unowned)+1))
				if err != nil {
					return nil, nil, 0, err
				}
				unowned[key] = fixture
			}
		}
		update := threadPathUpdate{id: id, workspace: fixture.dir}
		if strings.TrimSpace(worktree) != "" {
			fixtureWorktree, created, err := ensureFixtureWorktree(fixture, worktreesDir, worktree)
			if err != nil {
				return nil, nil, 0, err
			}
			if created {
				worktrees++
			}
			byRealWorktree[worktree] = fixtureWorktree
			update.workspace = fixtureWorktree
			update.worktree = fixtureWorktree
			update.hasWorktree = true
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("read threads: %w", err)
	}
	return updates, byRealWorktree, worktrees, nil
}

func ensureFixtureWorktree(fixture *projectFixture, worktreesDir, realPath string) (string, bool, error) {
	if existing, ok := fixture.worktrees[realPath]; ok {
		return existing, false, nil
	}
	base := safePathSegment(pathBase(realPath))
	if base == "" {
		base = "worktree"
	}
	dirName := uniqueName(fixture.usedNames, base)
	dir := filepath.Join(worktreesDir, fixture.dirName, dirName)
	branch := uniqueName(fixture.usedBranch, branchName(dirName))
	if err := harness.AddWorktree(fixture.dir, dir, branch); err != nil {
		return "", false, fmt.Errorf("create worktree fixture %s: %w", dir, err)
	}
	fixture.worktrees[realPath] = dir
	return dir, true, nil
}

func applyProjectPaths(db *sql.DB, updates []projectPathUpdate) error {
	return withoutTableTriggers(db, "projects", func() error {
		statement, err := db.Prepare(`UPDATE projects SET path = ? WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("prepare project path update: %w", err)
		}
		defer statement.Close()
		for _, update := range updates {
			if _, err := statement.Exec(update.path, update.id); err != nil {
				return fmt.Errorf("relocate project %s: %w", update.id, err)
			}
		}
		return nil
	})
}

func applyThreadPaths(db *sql.DB, updates []threadPathUpdate) error {
	return withoutTableTriggers(db, "threads", func() error {
		// A thread with no worktree keeps whatever worktree_path spelling it
		// had (NULL or ''), because the column is nullable and rewriting it
		// would change a value the relocation has no reason to touch.
		workspaceOnly, err := db.Prepare(`UPDATE threads SET workspace_path = ? WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("prepare thread workspace update: %w", err)
		}
		defer workspaceOnly.Close()
		both, err := db.Prepare(`UPDATE threads SET workspace_path = ?, worktree_path = ? WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("prepare thread worktree update: %w", err)
		}
		defer both.Close()
		for _, update := range updates {
			if update.hasWorktree {
				if _, err := both.Exec(update.workspace, update.worktree, update.id); err != nil {
					return fmt.Errorf("relocate thread %s: %w", update.id, err)
				}
				continue
			}
			if _, err := workspaceOnly.Exec(update.workspace, update.id); err != nil {
				return fmt.Errorf("relocate thread %s: %w", update.id, err)
			}
		}
		return nil
	})
}

// relocateWorkItemWorktrees maps a workflow table's worktree_path through
// the worktrees the thread pass created, and clears the rest. A real
// worktree path names exactly one project, so the map is keyed by path.
// Clearing is safe: an item with no worktree falls back to its project,
// which is now a fixture too.
func relocateWorkItemWorktrees(e *env, db *sql.DB, targetDB, table string, byRealWorktree map[string]string) (scrubResult, error) {
	present, err := tableExists(db, table)
	if err != nil {
		return scrubResult{}, err
	}
	if !present {
		fmt.Fprintf(e.stderr, "ao-harness: %s has no %s table; no worktree paths to relocate there\n", targetDB, table)
		return scrubResult{What: table + " worktrees", Detail: "absent in this database"}, nil
	}
	if err := requireColumns(db, table, "worktree_path"); err != nil {
		return scrubResult{}, err
	}
	rows, err := db.Query(`SELECT DISTINCT worktree_path FROM "` + table + `"
	                        WHERE worktree_path IS NOT NULL AND TRIM(worktree_path) <> ''`)
	if err != nil {
		return scrubResult{}, fmt.Errorf("read %s worktree paths: %w", table, err)
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return scrubResult{}, fmt.Errorf("scan %s worktree path: %w", table, err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return scrubResult{}, fmt.Errorf("read %s worktree paths: %w", table, err)
	}
	rows.Close()

	var mapped, cleared, affected int64
	err = withoutTableTriggers(db, table, func() error {
		statement, err := db.Prepare(`UPDATE "` + table + `" SET worktree_path = ? WHERE worktree_path = ?`)
		if err != nil {
			return fmt.Errorf("prepare %s worktree update: %w", table, err)
		}
		defer statement.Close()
		for _, path := range paths {
			replacement, ok := byRealWorktree[path]
			result, err := statement.Exec(replacement, path)
			if err != nil {
				return fmt.Errorf("relocate %s worktree %s: %w", table, path, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("relocate %s worktree %s: %w", table, path, err)
			}
			affected += count
			if ok {
				mapped += count
			} else {
				cleared += count
			}
		}
		return nil
	})
	if err != nil {
		return scrubResult{}, err
	}
	return scrubResult{
		What:   table + " worktrees",
		Detail: fmt.Sprintf("%d row(s), %d mapped to a fixture worktree, %d cleared", affected, mapped, cleared),
		Rows:   affected,
	}, nil
}

// withoutTableTriggers runs fn with the table's triggers dropped and
// restores them afterwards, the same bypass the scrub uses for integrity
// triggers that reject a neutralized value.
func withoutTableTriggers(db *sql.DB, table string, fn func() error) error {
	stashed, err := stashTableTriggers(db, table)
	if err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return restoreTableTriggers(db, table, stashed)
}

// requireColumns fails loudly on a store this code does not understand,
// rather than relocating part of it and leaving real paths behind.
func requireColumns(db *sql.DB, table string, columns ...string) error {
	present, err := tableExists(db, table)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("the copy has no %s table; refusing to relocate workspaces in a store this is not", table)
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("read %s columns: %w", table, err)
	}
	defer rows.Close()
	have := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan %s column: %w", table, err)
		}
		have[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s columns: %w", table, err)
	}
	for _, column := range columns {
		if _, ok := have[column]; !ok {
			return fmt.Errorf("table %s has no %s column; refusing to relocate workspaces in a store this is not", table, column)
		}
	}
	return nil
}

// removeCloneFixtureTree deletes one generated fixture tree. It refuses
// anything that is not strictly inside the target root, and refuses a
// symlink rather than deleting through it.
func removeCloneFixtureTree(targetRoot, dir string) error {
	if sameResolvedPath(targetRoot, dir) || !underDir(dir, targetRoot) {
		return fmt.Errorf("refusing to remove %s: it is not inside the target data root %s", dir, targetRoot)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fixture tree %s is a symlink; refusing to remove it", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}

// fixtureDirName picks the directory a project's fixture gets. slug is
// first because the store already keeps it unique.
func fixtureDirName(slug, name, path string) string {
	for _, candidate := range []string{slug, name, pathBase(path)} {
		if safe := safePathSegment(candidate); safe != "" {
			return safe
		}
	}
	return "workspace"
}

// safePathSegment reduces a value to one filesystem-safe path component.
func safePathSegment(value string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			out.WriteRune(r)
		case r == '.':
			// A lone dot is fine inside a name; runs of them are how "." and
			// ".." get in, and git also rejects ".." in a ref.
			if segment := out.String(); segment != "" && !strings.HasSuffix(segment, ".") {
				out.WriteRune(r)
			}
		default:
			if segment := out.String(); segment != "" && !strings.HasSuffix(segment, "-") {
				out.WriteRune('-')
			}
		}
	}
	trimmed := strings.Trim(out.String(), "-.")
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// branchName adapts a directory name to git's ref rules.
func branchName(dirName string) string {
	name := safePathSegment(dirName)
	if name == "" {
		name = "work"
	}
	if strings.HasSuffix(name, ".lock") {
		name += "-branch"
	}
	return name
}

// pathBase is filepath.Base for a path recorded on another platform: a
// cloned store can carry Windows spellings a POSIX filepath.Base treats as
// one component.
func pathBase(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), `/\`)
	if index := strings.LastIndexAny(trimmed, `/\`); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

func uniqueName(used map[string]struct{}, base string) string {
	name := base
	for suffix := 2; ; suffix++ {
		if _, taken := used[name]; !taken {
			used[name] = struct{}{}
			return name
		}
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
}
