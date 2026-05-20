package mcp

import "testing"

func TestSpecCacheKeyScopedByProvider(t *testing.T) {
	a := Spec{Provider: "claude", Name: "github"}
	b := Spec{Provider: "codex", Name: "github"}
	if a.CacheKey() == b.CacheKey() {
		t.Fatalf("same name across providers must not collide: %q == %q", a.CacheKey(), b.CacheKey())
	}
	if a.CacheKey() != "claude:github" {
		t.Errorf("CacheKey = %q, want claude:github", a.CacheKey())
	}
}

func TestSpecCacheKeyAcceptsEmptyProvider(t *testing.T) {
	s := Spec{Name: "lonely"}
	if s.CacheKey() != ":lonely" {
		t.Errorf("CacheKey for empty provider = %q, want :lonely", s.CacheKey())
	}
}
