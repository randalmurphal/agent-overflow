package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestNewThreadDisabledMCPServersMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)

	names, found, err := s.GetNewThreadDisabledMCPServers("claude", "/work")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers: %v", err)
	}
	if found {
		t.Fatal("missing defaults should not be found")
	}
	if names != nil {
		t.Fatalf("missing defaults names = %v, want nil", names)
	}
}

func TestSetNewThreadDisabledMCPServersNormalizesNames(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetNewThreadDisabledMCPServers("codex", "/ignored", []string{
		" server-b ",
		"",
		"server-a",
		"server-b",
	}); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers: %v", err)
	}
	names, found, err := s.GetNewThreadDisabledMCPServers("codex", "")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers: %v", err)
	}
	if !found {
		t.Fatal("defaults should be found after set")
	}
	want := []string{"server-a", "server-b"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestSetNewThreadDisabledMCPServersPersistsEmptySet(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetNewThreadDisabledMCPServers("claude", "/work", nil); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers(nil): %v", err)
	}
	names, found, err := s.GetNewThreadDisabledMCPServers("claude", "/work")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers: %v", err)
	}
	if !found {
		t.Fatal("empty defaults should still be snapshotted")
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want empty", names)
	}
}

func TestRemoveNewThreadDisabledMCPServerDropsStaleName(t *testing.T) {
	s := newTestStore(t)
	for _, workspace := range []string{"/work-a", "/work-b"} {
		if err := s.SetNewThreadDisabledMCPServers("claude", workspace, []string{"server-a", "server-b"}); err != nil {
			t.Fatalf("seed defaults %s: %v", workspace, err)
		}
	}

	if err := s.RemoveNewThreadDisabledMCPServer("claude", "server-a"); err != nil {
		t.Fatalf("RemoveNewThreadDisabledMCPServer: %v", err)
	}
	for _, workspace := range []string{"/work-a", "/work-b"} {
		names, found, err := s.GetNewThreadDisabledMCPServers("claude", workspace)
		if err != nil {
			t.Fatalf("GetNewThreadDisabledMCPServers %s: %v", workspace, err)
		}
		if !found {
			t.Fatalf("defaults for %s should remain after remove", workspace)
		}
		want := []string{"server-b"}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("names for %s = %v, want %v", workspace, names, want)
		}
	}
}

func TestNewThreadDisabledMCPServersScopesClaudeByWorkspace(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetNewThreadDisabledMCPServers("claude", "/work-a", []string{"server-a"}); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers work-a: %v", err)
	}
	if err := s.SetNewThreadDisabledMCPServers("claude", "/work-b", []string{"server-b"}); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers work-b: %v", err)
	}

	names, found, err := s.GetNewThreadDisabledMCPServers("claude", "/work-a")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers work-a: %v", err)
	}
	if !found || !reflect.DeepEqual(names, []string{"server-a"}) {
		t.Fatalf("work-a defaults = %v found=%v, want [server-a] found=true", names, found)
	}
	names, found, err = s.GetNewThreadDisabledMCPServers("claude", "/work-b")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers work-b: %v", err)
	}
	if !found || !reflect.DeepEqual(names, []string{"server-b"}) {
		t.Fatalf("work-b defaults = %v found=%v, want [server-b] found=true", names, found)
	}
}

func TestNewThreadDisabledMCPServersScopesCodexGlobally(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetNewThreadDisabledMCPServers("codex", "/work-a", []string{"server-a"}); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers work-a: %v", err)
	}
	if err := s.SetNewThreadDisabledMCPServers("codex", "/work-b", []string{"server-b"}); err != nil {
		t.Fatalf("SetNewThreadDisabledMCPServers work-b: %v", err)
	}

	names, found, err := s.GetNewThreadDisabledMCPServers("codex", "")
	if err != nil {
		t.Fatalf("GetNewThreadDisabledMCPServers: %v", err)
	}
	if !found || !reflect.DeepEqual(names, []string{"server-b"}) {
		t.Fatalf("codex defaults = %v found=%v, want [server-b] found=true", names, found)
	}
}

func TestNewThreadDisabledMCPServersRejectsInvalidProvider(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetNewThreadDisabledMCPServers("bad-provider", "", []string{"x"}); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("SetNewThreadDisabledMCPServers error = %v, want ErrInvalidProvider", err)
	}
	if _, _, err := s.GetNewThreadDisabledMCPServers("bad-provider", ""); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("GetNewThreadDisabledMCPServers error = %v, want ErrInvalidProvider", err)
	}
}

func TestMutateNewThreadDisabledMCPServersUsesFallbackOnlyWhenMissing(t *testing.T) {
	s := newTestStore(t)

	updated, err := s.MutateNewThreadDisabledMCPServers("claude", "/work", []string{"server-a"}, func(names []string) []string {
		return append(names, "server-b")
	})
	if err != nil {
		t.Fatalf("MutateNewThreadDisabledMCPServers initial: %v", err)
	}
	if !reflect.DeepEqual(updated, []string{"server-a", "server-b"}) {
		t.Fatalf("initial updated = %v, want [server-a server-b]", updated)
	}

	updated, err = s.MutateNewThreadDisabledMCPServers("claude", "/work", []string{"fallback"}, func(names []string) []string {
		return append(names, "server-c")
	})
	if err != nil {
		t.Fatalf("MutateNewThreadDisabledMCPServers existing: %v", err)
	}
	if !reflect.DeepEqual(updated, []string{"server-a", "server-b", "server-c"}) {
		t.Fatalf("existing updated = %v, want [server-a server-b server-c]", updated)
	}
}
