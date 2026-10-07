package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepositoryIdentityMigrationPreservesExistingProjects(t *testing.T) {
	db := migrateThrough(t, 140)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at,archived,remote_url,root_commit,identity_error) VALUES ('p','/old','Custom','custom',7,11,22,1,'https://user:SECRET@github.com/Owner/Repo.git?token=QUERY','root','failed https://user:SECRET@github.com/Owner/Repo.git')`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at,created_branch,created_remote_url,created_head_commit) VALUES ('t','p','Keep me','claude','model','/old',33,44,'feature','https://user:SECRET@github.com/old/name.git?token=QUERY','head')`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,tool_name,meta,created_at,updated_at) VALUES ('t','message',0,0,'user_text','user','completed','Existing conversation','','{}',33,33)`)
	var historyRev int64
	if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id='t'`).Scan(&historyRev); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, migrationByVersion(t, 141)); err != nil {
		t.Fatal(err)
	}
	var name, path, slug, remote, root, problem, id string
	var sort, created, updated, archived int
	if err := db.QueryRow(`SELECT name,path,slug,sort_position,created_at,updated_at,archived,remote_url,root_commit,identity_error,repository_id FROM projects WHERE id='p'`).Scan(&name, &path, &slug, &sort, &created, &updated, &archived, &remote, &root, &problem, &id); err != nil {
		t.Fatal(err)
	}
	if name != "Custom" || path != "/old" || slug != "custom" || sort != 7 || created != 11 || updated != 22 || archived != 1 || root != "" || id != "" {
		t.Fatal("migration changed project identity or presentation")
	}
	if remote != "" || strings.Contains(problem, "github.com") || strings.Contains(problem, "SECRET") {
		t.Fatal("credentials survived")
	}
	var project, title, branch, head string
	if err := db.QueryRow(`SELECT project_id,title,created_branch,created_remote_url,created_head_commit,updated_at FROM threads WHERE id='t'`).Scan(&project, &title, &branch, &remote, &head, &updated); err != nil {
		t.Fatal(err)
	}
	if project != "p" || title != "Keep me" || branch != "feature" || head != "head" || updated != 44 || remote != "" {
		t.Fatal("thread history or coordinates changed")
	}
	var summary string
	var afterRev int64
	if err := db.QueryRow(`SELECT summary FROM items WHERE thread_id='t' AND id='message'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT history_rev FROM threads WHERE id='t'`).Scan(&afterRev); err != nil {
		t.Fatal(err)
	}
	if summary != "Existing conversation" || historyRev != afterRev {
		t.Fatal("migration changed conversation history")
	}

}

func TestRepositoryURLsAreAbsentFromStoreAndWire(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	raw := "https://user:SECRET@github.com/a/b.git?token=QUERY"
	p, err := s.CreateProject(Project{ID: "p", Name: "p", Path: "/p", RepositoryID: "github:github.com:1", IdentitySource: "private-origin-stamp", IdentityError: "failed " + raw})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.IdentityError, "SECRET") {
		t.Fatal("project contains credentials")
	}
	if _, _, err := s.UpdateProjectIdentity(p.ID, ProjectIdentity{RepositoryID: p.RepositoryID, Error: raw}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(Thread{ID: "t", ProjectID: p.ID, Provider: "claude", Model: "model", WorkspacePath: "/p", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	thread, err := s.GetThread("t")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal([]any{p, thread})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "remoteURL") || strings.Contains(string(wire), "remoteUrl") || strings.Contains(string(wire), "rootCommit") || strings.Contains(string(wire), "identitySource") || strings.Contains(string(wire), "github.com/a/b") {
		t.Fatalf("retired coordinates on wire: %s", wire)
	}
	var remote, root, origin string
	if err := s.db.QueryRow(`SELECT p.remote_url,p.root_commit,t.created_remote_url FROM projects p JOIN threads t ON t.project_id=p.id WHERE p.id='p'`).Scan(&remote, &root, &origin); err != nil {
		t.Fatal(err)
	}
	if remote != "" || root != "" || origin != "" {
		t.Fatal("retired coordinates were persisted")
	}
	rows, err := s.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatal(err)
	}
	var found *Project
	for _, row := range rows {
		if row.Project.ID == p.ID {
			copy := row.Project
			found = &copy
		}
	}
	if found == nil || found.RepositoryID != p.RepositoryID || strings.Contains(found.IdentityError, "SECRET") {
		t.Fatalf("project list lost ID or leaked credentials: rows=%+v created=%+v", rows, p)
	}
}
