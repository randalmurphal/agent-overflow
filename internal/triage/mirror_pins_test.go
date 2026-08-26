package triage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// repoRelativePath resolves a repo-relative path from this source file's own
// location rather than the working directory, which `go test` sets to the
// package directory.
func repoRelativePath(t *testing.T, rel string) string {
	t.Helper()
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the repository root: runtime.Caller failed")
	}
	// internal/triage/<this file> → repository root.
	root := filepath.Join(filepath.Dir(sourcePath), "..", "..")
	return filepath.Join(root, filepath.FromSlash(rel))
}

// goStringConstants parses one Go source file and returns the untyped string
// constants it declares, by name. It exists so triage can pin a vocabulary it
// shares with a provider package WITHOUT importing that package — triage is
// provider-agnostic by contract (see the area guide's responsibility
// boundary), and the constants on the other side are unexported.
func goStringConstants(t *testing.T, rel string) map[string]string {
	t.Helper()
	path := repoRelativePath(t, rel)
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	out := map[string]string{}
	for _, decl := range parsed.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != len(valueSpec.Values) {
				continue
			}
			for i, name := range valueSpec.Names {
				literal, ok := valueSpec.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					continue
				}
				out[name.Name] = value
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no string constants found — this pin cannot find its mirror", rel)
	}
	return out
}

// The permission-notice kind vocabulary is duplicated across the provider
// boundary on purpose, exactly like the peer-turn origin above it: triage may
// not reach into a provider package for a term. What it may not do either is
// let the two drift — the discriminator is what routes the sanitizer and what
// the frontend branches on, so a rename on one side silently produces rows
// nothing renders.
func TestPermissionNoticeKindsMatchTheProviderConstants(t *testing.T) {
	const mirror = "internal/provider/claude/parse_system.go"
	constants := goStringConstants(t, mirror)
	for _, pair := range []struct {
		provider string
		triage   string
	}{
		{"permissionDeniedNoticeKind", permissionDeniedNotificationKind},
		{"permissionRetryNoticeKind", permissionRetryNotificationKind},
	} {
		value, ok := constants[pair.provider]
		if !ok {
			t.Errorf("%s no longer declares %s", mirror, pair.provider)
			continue
		}
		if value != pair.triage {
			t.Errorf("%s %s = %q, triage has %q", mirror, pair.provider, value, pair.triage)
		}
	}
}

// The model-fallback subtypes are the wire's own `system` subtypes, forwarded
// verbatim as the persisted notification kind. Pinned to the parser's own
// switch rather than to a constant, because upstream names them inline.
func TestModelFallbackKindsMatchTheParserSubtypes(t *testing.T) {
	const mirror = "internal/provider/claude/parse_system.go"
	source, err := os.ReadFile(repoRelativePath(t, mirror))
	if err != nil {
		t.Fatalf("read %s: %v", mirror, err)
	}
	text := string(source)
	for _, kind := range []string{
		modelRefusalFallbackNotificationKind,
		modelAvailabilityFallbackKind,
		modelConsentFallbackKind,
	} {
		if !strings.Contains(text, `"`+kind+`"`) {
			t.Errorf("%s no longer mentions the %q subtype triage persists", mirror, kind)
		}
	}
}

func TestCodexLatestToolTrayMetaKeysMatchFrontendMirror(t *testing.T) {
	const backend = "internal/store/subagent_items.go"
	const frontend = "frontend/src/lib/utils/codexTrayProjection.ts"
	backendValues := goStringConstants(t, backend)
	frontendSource, err := os.ReadFile(repoRelativePath(t, frontend))
	if err != nil {
		t.Fatalf("read %s: %v", frontend, err)
	}
	for _, name := range []string{
		"metaKeySubagentLatestToolSummary",
		"metaKeySubagentLatestToolTurn",
		"metaKeySubagentLatestToolItem",
	} {
		value, ok := backendValues[name]
		if !ok || value == "" {
			t.Fatalf("%s no longer declares %s", backend, name)
		}
		if !strings.Contains(string(frontendSource), `'`+value+`'`) {
			t.Errorf("%s does not mirror %s = %q", frontend, name, value)
		}
	}
}

// The Codex adapter puts a `userMessage` echo's `clientId` on the event meta
// under its own unexported constant, and triage reads it back by name to
// decide which pending send an echo belongs to. Two spellings of one key with
// no compiler relationship between them: a rename on the provider side would
// silently return triage to FIFO matching, which is the exact mispop the
// client-id path exists to prevent — and nothing would fail except a user's
// transcript, in production, on a thread with a foreign queue row in it.
func TestUserEchoClientIDKeyMatchesTheProviderConstant(t *testing.T) {
	const rel = "internal/provider/codex/protocol_item.go"
	constants := goStringConstants(t, rel)
	provider, ok := constants["userMessageClientIDMetaKey"]
	if !ok {
		t.Fatalf("%s no longer declares userMessageClientIDMetaKey — find where the echo's clientId lands now", rel)
	}
	if provider != userEchoClientIDMetaKey {
		t.Fatalf("meta key drift: triage reads %q, %s writes %q", userEchoClientIDMetaKey, rel, provider)
	}
}
