package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The Claude session-axis editor validates before it saves, so every bound
// in claudeSessionAxes.ts is a hand-written copy of a constant in
// claudesession.go. The two are load-bearing in OPPOSITE directions and
// neither side can import the other:
//
//   - a frontend bound that is LOOSER than Go's turns a refused save into a
//     toast the user cannot act on ("must be between 1 and 512" for a field
//     the form said was fine);
//   - a frontend bound that is TIGHTER silently makes a value unreachable
//     that the backend, the CLI, and the settings file all accept.
//
// Same two-sided-copy pin as TestReservedEnvNamesMatchTheProviderPins and
// TestPlaceholderTokensMatchTheFrontendMirror. Reading the numbers out of
// the mirror rather than restating them here is the point: a third copy
// would drift exactly the way this test exists to catch.
const claudeSessionAxesMirror = "frontend/src/lib/utils/claudeSessionAxes.ts"

func TestClaudeSessionAxisBoundsMatchTheFrontendMirror(t *testing.T) {
	source := readRepoFileForMirror(t, claudeSessionAxesMirror)

	for _, tc := range []struct {
		export string
		want   int
		owner  string
	}{
		{"CLAUDE_SUBAGENT_LIMIT_MAX", MaxClaudeSubagentLimit, "MaxClaudeSubagentLimit"},
		{"CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN", MaxClaudeToolMemoryLimitLen, "MaxClaudeToolMemoryLimitLen"},
		{"CLAUDE_THINKING_BUDGET_MIN", MinClaudeThinkingBudgetTokens, "MinClaudeThinkingBudgetTokens"},
		{"CLAUDE_THINKING_BUDGET_MAX", MaxClaudeThinkingBudgetTokens, "MaxClaudeThinkingBudgetTokens"},
	} {
		got := numericExportFromMirror(t, source, tc.export)
		if got != tc.want {
			t.Errorf("%s exports %s = %d, want %d (%s)",
				claudeSessionAxesMirror, tc.export, got, tc.want, tc.owner)
		}
	}
}

// The tool-memory grammar is the one axis where a mismatch is invisible
// rather than merely annoying: a value the CLI does not match is IGNORED,
// so a frontend regex that admits something claudeToolMemoryLimitSize
// refuses (or vice versa) ends with a limit the user set and the CLI never
// installed.
func TestClaudeToolMemoryGrammarMatchesTheFrontendMirror(t *testing.T) {
	source := readRepoFileForMirror(t, claudeSessionAxesMirror)

	literal := regexp.MustCompile(`(?:export )?const TOOL_MEMORY_SIZE\s*=\s*/(.*)/([a-z]*);`)
	match := literal.FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("%s declares no TOOL_MEMORY_SIZE regex literal", claudeSessionAxesMirror)
	}
	body, flags := match[1], match[2]

	// Go spells case-insensitivity as an inline (?i) group because
	// regexp.MustCompile takes no flags; JS spells it as the trailing /i.
	// Strip the one to compare against the other rather than teaching either
	// side the other's syntax.
	const caseInsensitive = "(?i)"
	goSource := claudeToolMemoryLimitSize.String()
	if len(goSource) < len(caseInsensitive)+1 || goSource[1:1+len(caseInsensitive)] != caseInsensitive {
		t.Fatalf("claudeToolMemoryLimitSize = %q, want a leading ^%s", goSource, caseInsensitive)
	}
	goBody := "^" + goSource[1+len(caseInsensitive):]

	if goBody != body {
		t.Errorf("%s TOOL_MEMORY_SIZE = /%s/, want /%s/ (claudeToolMemoryLimitSize)",
			claudeSessionAxesMirror, body, goBody)
	}
	if !strings.Contains(flags, "i") {
		t.Errorf("%s TOOL_MEMORY_SIZE carries flags %q, want the i flag — the CLI's own grammar is case-insensitive",
			claudeSessionAxesMirror, flags)
	}
}

// readRepoFileForMirror resolves a repo-relative path from this source
// file's own location, not the working directory: `go test` sets that to
// the package directory.
func readRepoFileForMirror(t *testing.T, rel string) string {
	t.Helper()
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the repository root: runtime.Caller failed")
	}
	// internal/settings/<this file> → repository root.
	root := filepath.Join(filepath.Dir(sourcePath), "..", "..")
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func numericExportFromMirror(t *testing.T, source, name string) int {
	t.Helper()
	pattern := regexp.MustCompile(fmt.Sprintf(`export const %s\s*=\s*(\d+);`, regexp.QuoteMeta(name)))
	match := pattern.FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("%s exports no numeric const %s", claudeSessionAxesMirror, name)
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("%s: %s = %q is not an integer: %v", claudeSessionAxesMirror, name, match[1], err)
	}
	return value
}
