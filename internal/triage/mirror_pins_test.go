package triage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/testutil/reporoot"
)

// goStringConstants parses one Go source file and returns the untyped string
// constants it declares, by name. It exists so triage can pin a vocabulary it
// shares with a provider package WITHOUT importing that package — triage is
// provider-agnostic by contract (see the area guide's responsibility
// boundary), and the constants on the other side are unexported.
func goStringConstants(t *testing.T, rel string) map[string]string {
	t.Helper()
	path := reporoot.Path(t, rel)
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
// boundary on purpose, exactly like the turn origins: triage may
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
	source, err := os.ReadFile(reporoot.Path(t, mirror))
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

// Store meta keys the frontend reads back by name, each pinned against the
// frontend module that spells them. A parked stop's sibling, for one,
// records its run in stored meta and its status, and the timeline reads both
// back by these names.
func TestStoreMetaKeysMatchFrontendMirrors(t *testing.T) {
	for _, mirror := range []struct {
		backend  string
		frontend string
		names    []string
	}{
		{
			backend:  "internal/store/subagent_items.go",
			frontend: "frontend/src/lib/utils/codexTrayProjection.ts",
			names: []string{
				"metaKeySubagentLatestToolSummary",
				"metaKeySubagentLatestToolTurn",
				"metaKeySubagentLatestToolItem",
			},
		},
		{
			backend:  "internal/store/background_tray_run_state.go",
			frontend: "frontend/src/lib/utils/subagentRunState.ts",
			names: []string{
				"MetaKeySubagentRunState",
				"MetaKeySubagentParkedCommands",
				"MetaKeySubagentParkedReportID",
				"MetaKeySubagentParkedReportPreview",
				"AgentRunRunning",
				"AgentRunParked",
				"AgentRunDone",
				"AgentRunEnded",
			},
		},
		{
			backend:  "internal/store/agent_stops.go",
			frontend: "frontend/src/lib/utils/parkedStop.ts",
			names:    []string{"MetaKeyParkedCommands", "MetaKeyParkedReportItemID", "MetaKeyRunStartedAt", "MetaKeyRunWoke", "ItemStatusParked"},
		},
	} {
		t.Run(mirror.frontend, func(t *testing.T) {
			backendValues := goStringConstants(t, mirror.backend)
			frontendSource, err := os.ReadFile(reporoot.Path(t, mirror.frontend))
			if err != nil {
				t.Fatalf("read %s: %v", mirror.frontend, err)
			}
			for _, name := range mirror.names {
				value, ok := backendValues[name]
				if !ok || value == "" {
					t.Fatalf("%s no longer declares %s", mirror.backend, name)
				}
				if !strings.Contains(string(frontendSource), `'`+value+`'`) {
					t.Errorf("%s does not mirror %s = %q", mirror.frontend, name, value)
				}
			}
		})
	}
}

// The origin vocabulary is duplicated across the provider boundary on
// purpose (triage is provider-agnostic by contract and may not reach into a
// provider package for a term), so each triage origin is pinned against the
// provider constant it mirrors. The frontend branches on and badges rows by
// these exact strings.
func TestTurnOriginsMatchTheProviderConstants(t *testing.T) {
	for _, pin := range []struct {
		name     string
		triage   string
		provider string
	}{
		{"claude.PeerTurnOrigin", crossSessionMessageOrigin, claude.PeerTurnOrigin},
		{"claude.ScheduledTurnOrigin", scheduledTurnOrigin, claude.ScheduledTurnOrigin},
		{"codex.ExternalTurnOriginQueue", externalQueueOrigin, codex.ExternalTurnOriginQueue},
	} {
		if pin.triage != pin.provider {
			t.Errorf("triage origin %q != %s %q", pin.triage, pin.name, pin.provider)
		}
	}
}

// The store finds an agent's wakes by the marker the parser writes on the
// wake row, under its own spelling: the store does not import provider.
func TestStoreWakeMarkerMatchesTheProviderKey(t *testing.T) {
	const rel = "internal/store/agent_stops.go"
	if got := goStringConstants(t, rel)["wakePromptMetaKey"]; got != provider.MetaSubagentWakePromptKey {
		t.Fatalf("%s wakePromptMetaKey = %q, provider.MetaSubagentWakePromptKey = %q", rel, got, provider.MetaSubagentWakePromptKey)
	}
}
