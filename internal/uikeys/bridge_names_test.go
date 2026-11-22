package uikeys

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"agent-overflow/internal/webview2host"
)

// The SPA posts its pane geometry, focus intents and shell chords to the
// window it runs in by message prefix, after checking a window property the
// window sets. The SPA's sources spell those names on their own, so this
// reads them and fails when a name differs from the window's.
func TestTheSPASpellsTheWindowsBridgeNames(t *testing.T) {
	for _, tc := range []struct {
		source string
		names  map[string]string
		// messages are message prefixes the source builds from its
		// MESSAGE_PREFIX, each a literal following ${MESSAGE_PREFIX}.
		messages []string
	}{
		{
			source: "../../frontend/src/lib/native/paneGeometryBridge.ts",
			names: map[string]string{
				"MESSAGE_PREFIX":          webview2host.GeometryMessagePrefix,
				"CAPABILITY_GLOBAL":       webview2host.GeometryCapabilityGlobal,
				"FOCUS_MESSAGE_PREFIX":    webview2host.FocusMessagePrefix,
				"FOCUS_CAPABILITY_GLOBAL": webview2host.FocusCapabilityGlobal,
			},
		},
		{
			source: "../../frontend/src/lib/native/shellChords.svelte.ts",
			names: map[string]string{
				"MESSAGE_PREFIX":    MessagePrefix,
				"CAPABILITY_GLOBAL": CapabilityGlobal,
				"READY_EVENT":       ReadyEvent,
				"HANDLER_GLOBAL":    HandlerGlobal,
			},
			messages: []string{routeMessage, reloadAppMessage},
		},
	} {
		body, err := os.ReadFile(tc.source)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for name, want := range tc.names {
			match := regexp.MustCompile(`(?m)^const ` + name + ` = '([^']*)';`).FindStringSubmatch(text)
			if match == nil {
				t.Errorf("%s declares no %s", tc.source, name)
				continue
			}
			if match[1] != want {
				t.Errorf("%s spells %s %q, the window %q", tc.source, name, match[1], want)
			}
		}
		for _, message := range tc.messages {
			suffix := strings.TrimPrefix(message, tc.names["MESSAGE_PREFIX"])
			if !strings.Contains(text, "${MESSAGE_PREFIX}"+suffix) {
				t.Errorf("%s builds no %q message", tc.source, message)
			}
		}
	}
}
