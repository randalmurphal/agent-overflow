package provider

import (
	"slices"
	"testing"
)

// Each provider's versions order as numbers for every slug shape its
// catalogs carry: 5 < 5.5 < 5.10 and 4.8 < 5 for Claude; a dotted minor,
// a bare major and a suffix for Codex. The newest of the family wins
// whatever order the provider lists them in.
func TestResolveModelPicksTheNewestVersionOfAFamily(t *testing.T) {
	cases := []struct {
		provider, family string
		listed           []string
		want             string
	}{
		{"claude", "claude-opus", []string{"claude-opus-4-8", "claude-opus-5", "claude-opus-4-5"}, "claude-opus-5"},
		{"claude", "claude-opus", []string{"claude-opus-5", "claude-opus-5-5", "claude-opus-4-8"}, "claude-opus-5-5"},
		{"claude", "claude-opus", []string{"claude-opus-5-10", "claude-opus-5-5", "claude-opus-5"}, "claude-opus-5-10"},
		{"claude", "claude-opus", []string{"claude-opus-6", "claude-opus-5-10"}, "claude-opus-6"},
		{"claude", "claude-haiku", []string{"claude-haiku-4-5", "claude-haiku-5-5", "claude-opus-6"}, "claude-haiku-5-5"},
		{"claude-tui", "claude-fable", []string{"claude-fable-5", "claude-fable-5-2", "claude-fable-5-1"}, "claude-fable-5-2"},
		{"codex", "gpt-sol", []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6.2-sol", "gpt-6.1-sol", "gpt-6.2-astra"}, "gpt-6.2-sol"},
		{"codex", "gpt-luna", []string{"gpt-5.6-luna", "gpt-6-luna"}, "gpt-6-luna"},
		{"codex", "gpt-sol", []string{"gpt-5.10-sol", "gpt-5.6-sol"}, "gpt-5.10-sol"},
		{"codex", "gpt", []string{"gpt-5.4", "gpt-5.5", "gpt-5.4-mini", "gpt-5"}, "gpt-5.5"},
		{"codex", "gpt-codex", []string{"gpt-5-codex", "gpt-5.3-codex", "gpt-5.3-codex-spark"}, "gpt-5.3-codex"},
		{"codex", "gpt-codex-spark", []string{"gpt-5.3-codex", "gpt-5.3-codex-spark"}, "gpt-5.3-codex-spark"},
		{"codex", "gpt-cyber", []string{"gpt-5.5-cyber-preview", "gpt-5.6-cyber"}, "gpt-5.6-cyber"},
	}
	for _, tc := range cases {
		ref := ParseModelRef(tc.provider, tc.family)
		if !ref.IsFamily() {
			t.Fatalf("%s %q does not read as a family", tc.provider, tc.family)
		}
		got, ok := ResolveModel(ref, tc.listed, nil)
		if !ok || got != tc.want {
			t.Errorf("%s %s over %v = %q, %v; want %q", tc.provider, tc.family, tc.listed, got, ok, tc.want)
		}
	}
}

// A slug with a version, one the grammar cannot read, and any input of a
// provider name outside the enum are exact models; only a family named with
// no version is a family.
func TestParseModelRefReadsAFamilyOnlyWithoutAVersion(t *testing.T) {
	cases := []struct {
		provider, text string
		family         bool
	}{
		{"claude", "claude-opus", true},
		{"claude-tui", "claude-sonnet", true},
		{"claude", "claude-opus-5", false},
		{"claude", "claude-opus-5-5", false},
		{"claude", "claude-haiku-4-5-20251001", false},
		{"claude", "claude-opus-5[1m]", false},
		{"claude", "opus", false},
		{"claude", "gpt-sol", false},
		{"codex", "gpt-sol", true},
		{"codex", "gpt", true},
		{"codex", "gpt-codex-spark", true},
		{"codex", "gpt-6.2-sol", false},
		{"codex", "gpt-5.5", false},
		{"codex", "gpt-oss-120b", false},
		{"codex", "gpt-4o", false},
		{"codex", "o3", false},
		{"codex", "claude-opus", false},
		{"gemini", "gemini-pro", false},
		{"", "claude-opus", false},
	}
	for _, tc := range cases {
		ref := ParseModelRef(tc.provider, "  "+tc.text+" ")
		if ref.IsFamily() != tc.family || ref.String() != tc.text {
			t.Errorf("%s %q: family %v, text %q; want family %v", tc.provider, tc.text, ref.IsFamily(), ref.String(), tc.family)
		}
	}
	if !ParseModelRef("claude", "  ").IsZero() || !ExactModel("").IsZero() {
		t.Error("blank input is not the zero ref")
	}
	if ExactModel("claude-opus").IsFamily() {
		t.Error("an exact ref reads as a family")
	}
}

// An exact model the computer offers, listed or kept, is that model and
// never moves to a newer one; one it does not offer resolves to nothing.
func TestResolveModelNeverMovesAnExactModel(t *testing.T) {
	listed := []string{"claude-opus-5-5", "claude-opus-6"}
	kept := []string{"claude-opus-5"}
	for _, slug := range []string{"claude-opus-5-5", "claude-opus-5"} {
		if got, ok := ResolveModel(ParseModelRef("claude", slug), listed, kept); !ok || got != slug {
			t.Errorf("%s resolves to %q, %v", slug, got, ok)
		}
	}
	if got, ok := ResolveModel(ParseModelRef("claude", "claude-opus-4-8"), listed, kept); ok || got != "" {
		t.Errorf("an unoffered exact model resolves to %q", got)
	}
}

// The precedence: a slug that reads as a family but is itself offered is
// that model; a family never takes a kept model or a slug the grammar cannot
// read, and with no listed member resolves to nothing.
func TestResolveModelPrecedence(t *testing.T) {
	offered := []string{"gpt-sol", "gpt-6.2-sol"}
	if got, _ := ResolveModel(ParseModelRef("codex", "gpt-sol"), offered, nil); got != "gpt-sol" {
		t.Errorf("an offered slug that reads as a family resolves to %q, want itself", got)
	}
	if got, _ := ResolveModel(ParseModelRef("codex", "gpt-sol"), []string{"gpt-6.2-sol"}, []string{"gpt-sol"}); got != "gpt-sol" {
		t.Errorf("a kept slug that reads as a family resolves to %q, want itself", got)
	}
	if got, ok := ResolveModel(ParseModelRef("claude", "claude-opus"), []string{"claude-sonnet-5-5"}, []string{"claude-opus-5"}); ok {
		t.Errorf("a family with only a kept member resolves to %q", got)
	}
	listed := []string{"claude-opus-5-5", "claude-opus-6-preview", "claude-opus-7-20270101"}
	if got, _ := ResolveModel(ParseModelRef("claude", "claude-opus"), listed, nil); got != "claude-opus-5-5" {
		t.Errorf("unreadable slugs were chosen: %q", got)
	}
	if got, ok := ResolveModel(ModelRef{}, listed, nil); ok {
		t.Errorf("the zero ref resolves to %q", got)
	}
}

// The slugs a grammar cannot read are reported, in order; every model the
// shipped catalogs carry is readable.
func TestUnreadableModels(t *testing.T) {
	got := UnreadableModels("claude", []string{"claude-opus-5-5", "claude-opus-6-preview", "claude-haiku-4-5", "claude-opus-7-20270101"})
	if !slices.Equal(got, []string{"claude-opus-6-preview", "claude-opus-7-20270101"}) {
		t.Errorf("claude unreadable = %v", got)
	}
	if got := UnreadableModels("codex", []string{"gpt-6.2-sol", "o3", "gpt-oss-120b"}); !slices.Equal(got, []string{"o3", "gpt-oss-120b"}) {
		t.Errorf("codex unreadable = %v", got)
	}
	if got := UnreadableModels("gemini", []string{"anything"}); got != nil {
		t.Errorf("a provider with no grammar reports %v", got)
	}
	for _, name := range []string{string(Claude), string(ClaudeTUI), string(Codex)} {
		var slugs []string
		for _, model := range ModelsForProvider(name) {
			slugs = append(slugs, model.Slug)
		}
		if bad := UnreadableModels(name, slugs); len(bad) > 0 {
			t.Errorf("%s ships models its grammar cannot read: %v", name, bad)
		}
	}
}
