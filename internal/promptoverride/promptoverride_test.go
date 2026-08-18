package promptoverride

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/settings"
)

func TestMatchPicksFirstEnabledEntryForTheModel(t *testing.T) {
	entries := []settings.PromptOverride{
		{Enabled: false, Models: []string{"claude-opus-5"}, Prompt: "disabled"},
		{Enabled: true, Models: []string{"claude-fable-5"}, Prompt: "fable"},
		{Enabled: true, Models: []string{"claude-sonnet-5", "claude-opus-5"}, Prompt: "first winner"},
		{Enabled: true, Models: []string{"claude-opus-5"}, Prompt: "second"},
	}

	entry, ok := Match(entries, "claude", "claude-opus-5")
	if !ok {
		t.Fatal("Match() ok = false, want a match")
	}
	if entry.Prompt != "first winner" {
		t.Fatalf("prompt = %q, want the first enabled entry listing the model", entry.Prompt)
	}

	if _, ok := Match(entries, "claude", "claude-haiku-4-5"); ok {
		t.Fatal("Match() matched a model no entry lists")
	}
}

// The context-tier marker is a window, not an identity: a selection saved
// as `claude-opus-5` must match a thread launched on the 1M spelling, and
// a selection that somehow carries the marker must match the plain thread.
func TestMatchIgnoresTheContextTierMarker(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		model string
	}{
		{name: "marker on the thread", entry: "claude-opus-5", model: "claude-opus-5[1m]"},
		{name: "marker on the entry", entry: "claude-opus-5[1m]", model: "claude-opus-5"},
		{name: "marker on both", entry: "claude-opus-5[1m]", model: "claude-opus-5[1m]"},
		{name: "alias on the entry", entry: "opus", model: "claude-opus-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []settings.PromptOverride{
				{Enabled: true, Models: []string{tc.entry}, Prompt: "p"},
			}
			if _, ok := Match(entries, "claude", tc.model); !ok {
				t.Fatalf("Match(entry=%q, model=%q) = false, want true", tc.entry, tc.model)
			}
		})
	}
}

// Normalization is provider.NormalizeModelSlug's whole job, including which
// providers the marker rule applies to. This package must not layer a
// second rule on top: it once trimmed the marker itself before calling, so
// a bracketed CODEX id was trimmed here and nowhere else in the app.
func TestMatchNormalizesCodexIDsThroughTheProviderTable(t *testing.T) {
	entries := []settings.PromptOverride{
		{Enabled: true, Models: []string{"gpt-5-codex"}, Prompt: "p"},
	}
	if _, ok := Match(entries, "codex", "gpt-5.4"); !ok {
		t.Fatal("Match() = false for a codex alias its resolved id should match")
	}
}

func TestMatchSkipsEntriesThatCannotApply(t *testing.T) {
	cases := []struct {
		name    string
		entries []settings.PromptOverride
		model   string
	}{
		{
			name:    "disabled",
			entries: []settings.PromptOverride{{Models: []string{"gpt-5.6-sol"}, Prompt: "p"}},
			model:   "gpt-5.6-sol",
		},
		{
			name:    "blank prompt",
			entries: []settings.PromptOverride{{Enabled: true, Models: []string{"gpt-5.6-sol"}, Prompt: "   "}},
			model:   "gpt-5.6-sol",
		},
		{
			name:    "no models",
			entries: []settings.PromptOverride{{Enabled: true, Prompt: "p"}},
			model:   "gpt-5.6-sol",
		},
		{
			name:    "blank model on the thread",
			entries: []settings.PromptOverride{{Enabled: true, Models: []string{"gpt-5.6-sol"}, Prompt: "p"}},
			model:   "  ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := Match(tc.entries, "codex", tc.model); ok {
				t.Fatal("Match() matched an entry that cannot apply")
			}
		})
	}
}

// An entry Match cannot apply is SKIPPED, not matched-and-ignored: the walk
// continues past it. Otherwise a half-typed draft parked above a working
// entry would silently disable the working one — the failure a user would
// read as "my override stopped applying" with nothing on screen saying so.
func TestMatchWalksPastABlankDraftToAWorkingEntry(t *testing.T) {
	entries := []settings.PromptOverride{
		{Enabled: true, Models: []string{"gpt-5.6-sol"}, Prompt: "  "},
		{Enabled: true, Models: []string{"gpt-5.6-sol"}, Prompt: "real"},
	}
	entry, ok := Match(entries, "codex", "gpt-5.6-sol")
	if !ok {
		t.Fatal("Match() ok = false; a blank draft must not end the walk")
	}
	if entry.Prompt != "real" {
		t.Fatalf("prompt = %q, want the working entry below the blank draft", entry.Prompt)
	}

	// Same rule for the other skip reason, so neither `continue` can decay
	// into a return without a failing test.
	entries[0].Enabled = false
	entries[0].Prompt = "parked"
	if entry, ok := Match(entries, "codex", "gpt-5.6-sol"); !ok || entry.Prompt != "real" {
		t.Fatalf("Match() = (%q, %v), want the enabled entry below the disabled one", entry.Prompt, ok)
	}
}

func TestRenderSubstitutesKnownTokensOnly(t *testing.T) {
	facts := Facts{
		WorkDir:   "/w",
		IsGitRepo: "Yes",
		GitBlock:  "Current branch: main",
		Platform:  "linux",
		OSVersion: "ubuntu 24.04",
		ModelName: "Opus 5",
		ModelID:   "claude-opus-5",
		MemoryDir: "/home/u/.claude/projects/-w/memory",
	}
	prompt := strings.Join([]string{
		"cwd={{WORKDIR}}", "repo={{IS_GIT_REPO}}", "git={{GIT_BLOCK}}",
		"plat={{PLATFORM}}", "os={{OS_VERSION}}", "name={{MODEL_NAME}}",
		"id={{MODEL_ID}}", "mem={{MEMORY_DIR}}",
	}, "\n")

	got := Render(prompt, facts)
	want := strings.Join([]string{
		"cwd=/w", "repo=Yes", "git=Current branch: main",
		"plat=linux", "os=ubuntu 24.04", "name=Opus 5",
		"id=claude-opus-5", "mem=/home/u/.claude/projects/-w/memory",
	}, "\n")
	if got != want {
		t.Fatalf("Render() =\n%s\nwant\n%s", got, want)
	}
}

// A typo'd or unknown placeholder is left standing on purpose: the user
// sees it in the model's context and can fix it, which beats a silent
// deletion and beats failing the spawn.
func TestRenderLeavesUnknownPlaceholdersAndLiteralBracesAlone(t *testing.T) {
	prompt := "{{WORKDIRR}} {{ WORKDIR }} {{workdir}} {{TODAY}} {{ } literal {{ braces"
	if got := Render(prompt, Facts{WorkDir: "/w"}); got != prompt {
		t.Fatalf("Render() = %q, want the input unchanged", got)
	}
}

// One left-to-right pass: a fact that happens to contain a token is not
// rescanned, so a workspace path with braces cannot expand further.
func TestRenderDoesNotRescanSubstitutedValues(t *testing.T) {
	got := Render("{{WORKDIR}}", Facts{WorkDir: "/tmp/{{MODEL_ID}}", ModelID: "claude-opus-5"})
	if got != "/tmp/{{MODEL_ID}}" {
		t.Fatalf("Render() = %q, want the substituted value verbatim", got)
	}
}

func TestRenderEmptyFactsClearTokens(t *testing.T) {
	if got := Render("[{{GIT_BLOCK}}]", Facts{}); got != "[]" {
		t.Fatalf("Render() = %q, want the token cleared rather than left raw", got)
	}
}

func TestUsesReportsPlaceholderPresence(t *testing.T) {
	if !Uses("see {{MEMORY_DIR}} for notes", TokenMemoryDir) {
		t.Fatal("Uses() = false for a prompt that carries the token")
	}
	if Uses("see {{MEMORY}} for notes", TokenMemoryDir) {
		t.Fatal("Uses() = true for a prompt that does not carry the token")
	}
}

// The slug encoder is shared with the session-relocation writer, so the
// directory AO creates is the one a resumed CLI reads. Pin the layout.
func TestClaudeMemoryDirUsesTheClaudeProjectSlugLayout(t *testing.T) {
	home := t.TempDir()
	workDir := t.TempDir()

	dir, ok, err := ClaudeMemoryDir(home, workDir)
	if err != nil {
		t.Fatalf("ClaudeMemoryDir() error = %v", err)
	}
	if !ok {
		t.Fatalf("ClaudeMemoryDir() ok = false for %s", workDir)
	}
	if filepath.Base(dir) != "memory" {
		t.Fatalf("dir = %q, want a trailing memory component", dir)
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	if !strings.HasPrefix(dir, projectsDir+string(filepath.Separator)) {
		t.Fatalf("dir = %q, want it under %q", dir, projectsDir)
	}
	slug := filepath.Base(filepath.Dir(dir))
	for _, r := range slug {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' {
			t.Fatalf("slug %q contains %q — every non-alphanumeric byte must map to '-'", slug, r)
		}
	}
}

func TestClaudeMemoryDirErrorsOnAMissingWorkspace(t *testing.T) {
	if _, _, err := ClaudeMemoryDir(t.TempDir(), filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("ClaudeMemoryDir() error = nil for a workspace that does not exist")
	}
}

// The settings editor's placeholder legend is a hand-mirrored copy of the
// Token* constants, and the two sets have to be identical in both
// directions: a token only the legend knows renders literally into the
// prompt the model reads, and a token only Go knows is a substitution the
// user is never told exists. Same two-sided-copy pin as
// TestReservedEnvNamesMatchTheProviderPins, parsing the mirror the way
// internal/highlight pins its JS hash parity.
func TestPlaceholderTokensMatchTheFrontendMirror(t *testing.T) {
	const mirror = "frontend/src/lib/utils/promptOverrides.ts"
	source := readRepoFile(t, mirror)
	advertised := setOf(exportedArrayLiterals(t, source, mirror, "PROMPT_PLACEHOLDERS", `\btoken:\s*'([^']*)'`))
	// Read the constants out of the source rather than listing them here:
	// a third hand-written copy would drift exactly the way this test
	// exists to catch, and a token added to Go alone would go unnoticed.
	owned := goTokenConstants(t)

	for name, token := range owned {
		if _, ok := advertised[token]; !ok {
			t.Errorf("%s = %q is substituted but %s does not advertise it", name, token, mirror)
		}
	}
	for token := range advertised {
		found := false
		for _, owned := range owned {
			if owned == token {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s advertises %q but Render substitutes no such token — it would reach the model verbatim", mirror, token)
		}
	}
}

// goTokenConstants reads this package's own Token* constants out of the
// source, name → value.
func goTokenConstants(t *testing.T) map[string]string {
	t.Helper()

	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve promptoverride.go: runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(sourcePath), "promptoverride.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	tokens := map[string]string{}
	for _, decl := range file.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Token") || i >= len(value.Values) {
					continue
				}
				lit, isLit := value.Values[i].(*ast.BasicLit)
				if !isLit {
					t.Fatalf("%s is not a literal constant; the pin reads literals", name.Name)
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s = %s: %v", name.Name, lit.Value, err)
				}
				tokens[name.Name] = unquoted
			}
		}
	}
	if len(tokens) == 0 {
		t.Fatalf("%s: no Token* constants found — the pin would assert nothing", path)
	}
	return tokens
}

func setOf(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// readRepoFile reads a repo-relative path. The repository root is derived
// from this source file's own location rather than the working directory,
// which `go test` sets to the package directory.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the repository root: runtime.Caller failed")
	}
	// internal/promptoverride/<this file> → repository root.
	root := filepath.Join(filepath.Dir(sourcePath), "..", "..")
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// exportedArrayLiterals pulls every capture of pattern out of the
// `export const <name> = [ … ]` block of a TypeScript source. Anchoring on
// the const name and on the flush-left `]` that closes it keeps comments,
// blank lines and reformatting inside the array harmless; the file carries
// a comment saying a Go test parses it. Extracting nothing is a hard
// failure — a silently empty set would pin nothing while looking green.
func exportedArrayLiterals(t *testing.T, source, path, name, pattern string) []string {
	t.Helper()

	start := strings.Index(source, "export const "+name)
	if start < 0 {
		t.Fatalf("%s: no `export const %s` — this pin cannot find its mirror", path, name)
	}
	block := source[start:]
	open := strings.Index(block, "[")
	end := strings.Index(block, "\n]")
	if open < 0 || end < open {
		t.Fatalf("%s: `export const %s` is not a bracketed array literal", path, name)
	}
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(block[open:end], -1)
	if len(matches) == 0 {
		t.Fatalf("%s: %s matched no %s literals — the entry syntax the pin parses changed", path, name, pattern)
	}
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
}
