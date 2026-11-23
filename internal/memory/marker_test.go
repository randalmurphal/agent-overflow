package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A marker published by a hard link whose temporary name could not be
// removed is published: the folder records the project, and the call's own
// removal of the temporary file takes the leftover, or reports that it
// could not.
func TestRecordProjectPathTreatsALeftoverAsPublished(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		removeErr error
	}{
		{"removed", nil},
		{"still there", errors.New("injected removal failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			project := Project{Name: "app", Path: filepath.Join(t.TempDir(), "repo"), Dir: t.TempDir()}
			ops := markerOps{
				publish: func(from, to string) (string, error) {
					if err := os.Link(from, to); err != nil {
						return "", err
					}
					return from, nil
				},
				remove: func(name string) error {
					if tc.removeErr != nil {
						return tc.removeErr
					}
					return os.Remove(name)
				},
			}

			err := recordProjectPath(project, ops)
			marker, readErr := os.ReadFile(filepath.Join(project.Dir, ProjectPathFile))
			if readErr != nil || string(marker) != filepath.Clean(project.Path)+"\n" {
				t.Fatalf("marker = %q, %v; want the project's path", marker, readErr)
			}
			entries, _ := os.ReadDir(project.Dir)
			switch {
			case tc.removeErr == nil && (err != nil || len(entries) != 1):
				t.Fatalf("recordProjectPath = %v with %d entries, want the marker alone", err, len(entries))
			case tc.removeErr != nil && (err == nil || !strings.Contains(err.Error(), "injected removal failure")):
				t.Fatalf("recordProjectPath = %v, want the failed removal reported", err)
			}
		})
	}
}
