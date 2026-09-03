package projectapp

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/entityid"
)

// Project ids share the thread-id contract: a client attached to several
// backends (docs/specs/remote-access.md §10) keys projects by this string
// alone, so a path-derived or sequential id would collide across
// backends. Deriving it from the path is the tempting mistake here —
// two machines checked out at ~/repos/app are two projects.
func TestCreateMintsGloballyUniqueProjectID(t *testing.T) {
	service, _ := newTestService(t)

	ids := make(map[string]struct{}, 2)
	for _, name := range []string{"one", "two"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		project, err := service.Create(path)
		if err != nil {
			t.Fatalf("Create(%s): %v", path, err)
		}
		if !entityid.Valid(project.ID) {
			t.Fatalf("project id = %q, which is not a globally unique entity id", project.ID)
		}
		if _, dup := ids[project.ID]; dup {
			t.Fatalf("project id %q reused", project.ID)
		}
		ids[project.ID] = struct{}{}
	}
}
