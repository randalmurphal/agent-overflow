package transport

import (
	"os"
	"strings"
	"testing"
)

// TestFrontendEntityFilteredChannelsMatch pins
// frontend/src/lib/transport/entityFilteredChannels.ts to this package's
// EntityFiltered column, in both directions.
//
// The two lists are not redundant copies of one fact — they are two halves
// of one, and the failure modes are asymmetric enough that both directions
// have to be checked:
//
//   - A channel this package filters and the module omits is a RESYNC
//     STORM. A withheld frame still spent its channel's sequence number, so
//     the next delivered one skips forward; without the exemption the
//     client reads every skip as dropped events and answers with a full
//     resync — on the busiest channels in the app.
//   - A channel the module lists and this package does not filter is a
//     BLIND SPOT. The client would suppress a real drop's only signal on a
//     channel where nothing is being withheld.
//
// Textual, the same shape as TestFrontendScopeVocabularyMatches: the
// literal is a flat list of quoted strings by construction.
func TestFrontendEntityFilteredChannelsMatch(t *testing.T) {
	const modulePath = "../../frontend/src/lib/transport/entityFilteredChannels.ts"
	source, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatalf("read %s: %v", modulePath, err)
	}
	declared := frontendStringList(t, string(source),
		"export const ENTITY_FILTERED_CHANNELS: readonly string[] = [")

	want := EntityFilteredChannels()
	if len(declared) != len(want) {
		t.Fatalf("%s declares %d entity-filtered channels (%v), this package filters %d (%v)",
			modulePath, len(declared), declared, len(want), want)
	}
	for i, channel := range want {
		if declared[i] != channel {
			t.Errorf("position %d: %s declares %q, this package filters %q — the table is alphabetical and a move is drift",
				i, modulePath, declared[i], channel)
		}
	}
}

// frontendStringList extracts one flat array literal's quoted entries.
// Accepts either quote style, since this module is not the scope table and
// nothing pins its formatting.
func frontendStringList(t *testing.T, module, marker string) []string {
	t.Helper()
	start := strings.Index(module, marker)
	if start < 0 {
		t.Fatalf("no %q in the module; the gate is reading the wrong shape", marker)
	}
	rest := module[start+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("%q literal never closes", marker)
	}
	var names []string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if len(line) < 2 {
			continue
		}
		quote := line[0]
		if (quote == '\'' || quote == '"') && line[len(line)-1] == quote {
			names = append(names, line[1:len(line)-1])
		}
	}
	if len(names) == 0 {
		t.Fatalf("%q literal parsed to nothing; the gate is reading the wrong shape", marker)
	}
	return names
}
