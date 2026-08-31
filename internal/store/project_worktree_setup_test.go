package store

import (
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/worktreesetup"
)

// The accessor pair is state-bearing, so it is covered over TRANSITIONS —
// set → clear → set again, plus the clear-by-empty-config path — not just over
// each state once. A clear that left the previous recipe behind would run a
// stale setup on every worktree cut afterwards.
func TestProjectWorktreeSetupRoundTripsAndClears(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateProject(newProject("wp1", "/tmp/wp1", "WP1")); err != nil {
		t.Fatal(err)
	}

	config, found, err := s.ProjectWorktreeSetup("wp1")
	if err != nil || found || !config.IsZero() {
		t.Fatalf("fresh project setup = %+v, %v, %v", config, found, err)
	}

	first := worktreesetup.Config{
		Copy:    []string{".env", "config/*.local"},
		Run:     [][]string{{"pnpm", "install", "--frozen-lockfile"}},
		Timeout: "15m",
	}
	if _, _, err := s.UpdateProjectWorktreeSetup("wp1", &first); err != nil {
		t.Fatal(err)
	}
	config, found, err = s.ProjectWorktreeSetup("wp1")
	if err != nil || !found || !reflect.DeepEqual(config, first) {
		t.Fatalf("stored setup = %+v, %v, %v", config, found, err)
	}

	if _, _, err := s.UpdateProjectWorktreeSetup("wp1", nil); err != nil {
		t.Fatal(err)
	}
	config, found, err = s.ProjectWorktreeSetup("wp1")
	if err != nil || found || !config.IsZero() {
		t.Fatalf("cleared setup = %+v, %v, %v", config, found, err)
	}

	second := worktreesetup.Config{Run: [][]string{{"make", "install"}}}
	if _, _, err := s.UpdateProjectWorktreeSetup("wp1", &second); err != nil {
		t.Fatal(err)
	}
	config, found, err = s.ProjectWorktreeSetup("wp1")
	if err != nil || !found || !reflect.DeepEqual(config, second) {
		t.Fatalf("re-set setup = %+v, %v, %v", config, found, err)
	}

	// A config that asks for nothing is the same state as nil, so a later read
	// cannot report a configured-but-empty recipe.
	if _, _, err := s.UpdateProjectWorktreeSetup("wp1", &worktreesetup.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, found, err = s.ProjectWorktreeSetup("wp1"); err != nil || found {
		t.Fatalf("empty-config setup = %v, %v", found, err)
	}
}

func TestProjectWorktreeSetupRefusesACorruptBlob(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateProject(newProject("wp2", "/tmp/wp2", "WP2")); err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string]string{
		"not json":      `{copy: [".env"]}`,
		"unknown field": `{"copy":[".env"],"shell":"bash"}`,
		"wrong type":    `{"run":"make install"}`,
	} {
		if _, err := s.db.Exec(`UPDATE projects SET worktree_setup = ? WHERE id = ?`, blob, "wp2"); err != nil {
			t.Fatal(err)
		}
		config, found, err := s.ProjectWorktreeSetup("wp2")
		if err == nil {
			t.Fatalf("%s decoded to %+v, %v", name, config, found)
		}
		if !strings.Contains(err.Error(), "decode project") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

func TestUpdateProjectWorktreeSetupRefusesAnUnknownProject(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.UpdateProjectWorktreeSetup("missing", &worktreesetup.Config{Run: [][]string{{"make"}}}); err == nil {
		t.Fatal("update against a missing project succeeded")
	}
	if _, _, err := s.ProjectWorktreeSetup("missing"); err == nil {
		t.Fatal("read against a missing project succeeded")
	}
}
