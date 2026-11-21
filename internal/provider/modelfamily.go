package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Model families: which model is a provider's newest of a kind, decided by a
// rule Agent Overflow states per provider rather than by an alias the CLI
// reports (docs/decisions.md, Providers and accounts). A provider's grammar
// reads a slug as a family and a version; a model input that names a family
// without a version (`claude-opus`, `gpt-sol`) runs the highest version of
// that family the computer lists. The CLI only says which models exist.

// ModelRef is a model input once read by its provider's grammar: an exact
// model, or a family. The kind is fixed when it is parsed; ResolveModel is
// the one place that turns either into a model a computer offers.
type ModelRef struct {
	provider string
	text     string
	family   bool
}

// ParseModelRef reads text as providerName's grammar does. Text the grammar
// reads as a family with no version is a family; anything else, including a
// slug the grammar cannot read and every input of a provider whose models
// have no versioned families, is an exact model. Empty text is the zero ref:
// nothing named.
func ParseModelRef(providerName, text string) ModelRef {
	text = strings.TrimSpace(text)
	if text == "" {
		return ModelRef{}
	}
	ref := ModelRef{provider: providerName, text: text}
	if read := modelGrammarFor(ProviderKind(providerName)); read != nil {
		_, version, ok := read(text)
		ref.family = ok && version == nil
	}
	return ref
}

// ExactModel is a ref to the model slug names, never read as a family: a
// model a thread already runs, or a provider's default.
func ExactModel(slug string) ModelRef {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ModelRef{}
	}
	return ModelRef{text: slug}
}

// IsZero reports that the ref names nothing.
func (r ModelRef) IsZero() bool { return r.text == "" }

// IsFamily reports that the ref names a family rather than one model.
func (r ModelRef) IsFamily() bool { return r.family }

// String is the ref as it was written.
func (r ModelRef) String() string { return r.text }

// ResolveModel answers which of a computer's models ref runs, given the
// slugs its provider lists for a new thread and the ones it keeps only for
// threads already on them. This is the one statement of the precedence:
//
//  1. A model whose slug is ref as written wins, listed or kept. An exact
//     model never moves, and a slug that reads as a family but is itself
//     offered is that model.
//  2. A family runs its newest listed member by the provider's version
//     rule. A kept model is never chosen for a family, and neither is a
//     listed slug the rule cannot read.
//  3. Anything else resolves to nothing.
func ResolveModel(ref ModelRef, listed, kept []string) (string, bool) {
	if ref.IsZero() {
		return "", false
	}
	if slices.Contains(listed, ref.text) || slices.Contains(kept, ref.text) {
		return ref.text, true
	}
	if !ref.family {
		return "", false
	}
	read := modelGrammarFor(ProviderKind(ref.provider))
	best, bestVersion := "", []int(nil)
	for _, slug := range listed {
		family, version, ok := read(slug)
		if !ok || version == nil || family != ref.text {
			continue
		}
		if best == "" || compareModelVersions(version, bestVersion) > 0 {
			best, bestVersion = slug, version
		}
	}
	return best, best != ""
}

// UnreadableModels are the slugs providerName's grammar cannot read, in
// the order given. They are never chosen for a family, so a provider that
// changes how it spells a model is reported rather than silently left
// behind. A provider with no versioned families has none.
func UnreadableModels(providerName string, slugs []string) []string {
	read := modelGrammarFor(ProviderKind(providerName))
	if read == nil {
		return nil
	}
	var out []string
	for _, slug := range slugs {
		if _, _, ok := read(slug); !ok {
			out = append(out, slug)
		}
	}
	return out
}

// modelGrammar reads a slug as a family and a version. A family named with
// no version reads with a nil version; ok is false for a slug the provider's
// spelling does not cover.
type modelGrammar func(slug string) (family string, version []int, ok bool)

// modelGrammarFor is each provider's statement of how its slugs carry a
// family and a version. The switch names every provider and has no default,
// so a provider added to ProviderKind fails the exhaustive lint here until it
// states its grammar; one whose models have no versioned families returns
// nil, which makes its inputs exact-only. A name outside the enum is
// exact-only too.
func modelGrammarFor(kind ProviderKind) modelGrammar {
	switch kind {
	case Claude, ClaudeTUI:
		return readClaudeSlug
	case Codex:
		return readCodexSlug
	}
	return nil
}

// Claude: `claude-<name>-<major>[-<minor>]`, family `claude-<name>`
// (`claude-opus-5`, `claude-opus-5-5`, `claude-haiku-4-5`). The context
// marker and Haiku 4.5's dated ID are folded onto these slugs before any
// catalog lists them (NormalizeModelSlug), so neither is read here.
var claudeSlug = regexp.MustCompile(`^(claude-[a-z]+)(?:-([0-9]+)(?:-([0-9]{1,2}))?)?$`)

func readClaudeSlug(slug string) (string, []int, bool) {
	match := claudeSlug.FindStringSubmatch(slug)
	if match == nil {
		return "", nil, false
	}
	version, ok := versionOf(match[2], match[3])
	return match[1], version, ok
}

// Codex: `gpt-<major>[.<minor>][-<suffix>]`, family `gpt-<suffix>`, or
// `gpt` with no suffix (`gpt-6.2-sol` is `gpt-sol` 6.2, `gpt-5.5` is `gpt`
// 5.5, `gpt-5.3-codex-spark` is `gpt-codex-spark` 5.3).
var codexSlug = regexp.MustCompile(`^gpt(?:-([0-9]+)(?:\.([0-9]+))?)?((?:-[a-z][a-z0-9]*)*)$`)

func readCodexSlug(slug string) (string, []int, bool) {
	match := codexSlug.FindStringSubmatch(slug)
	if match == nil {
		return "", nil, false
	}
	version, ok := versionOf(match[1], match[2])
	return "gpt" + match[3], version, ok
}

// versionOf reads a major and an optional minor; no major is no version.
func versionOf(major, minor string) ([]int, bool) {
	if major == "" {
		return nil, true
	}
	version := make([]int, 0, 2)
	for _, part := range []string{major, minor} {
		if part == "" {
			continue
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		version = append(version, number)
	}
	return version, true
}

// compareModelVersions orders two versions part by part as numbers, a
// missing part reading as zero, so 5 < 5.5 < 5.10 and 4.8 < 5.
func compareModelVersions(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
