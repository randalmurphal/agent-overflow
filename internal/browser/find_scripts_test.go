package browser

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// updateFindScripts rewrites the scripts the frontend replays.
var updateFindScripts = os.Getenv("AO_UPDATE_FIND_SCRIPTS") == "1"

// findScriptsDir holds the scripts, one file per table.
const findScriptsDir = "../../frontend/src/test/fixtures/browserFind/"

// findScriptStep is one thing that happened to a provider, in order: what
// the reader did (UI, with Query for "search" and "type"), a typed query's
// debounce running out (Debounce), the page the pane now shows (Shown), a
// call the provider sent (Send), the answer to its oldest unanswered call
// (Answer, or Error), an end the thread's providers were told of (Event), or
// the provider's stop retry wait running out (Retry).
type findScriptStep struct {
	UI       string          `json:"ui,omitempty"`
	Query    string          `json:"query,omitempty"`
	Debounce bool            `json:"debounce,omitempty"`
	Shown    string          `json:"shown,omitempty"`
	Send     *findScriptCall `json:"send,omitempty"`
	Answer   *FindState      `json:"answer,omitempty"`
	Error    bool            `json:"error,omitempty"`
	Event    *FindState      `json:"event,omitempty"`
	Retry    bool            `json:"retry,omitempty"`
}

type findScriptCall struct {
	Kind  string `json:"kind"`
	Page  string `json:"page"`
	Seq   uint64 `json:"seq"`
	Query string `json:"query,omitempty"`
}

// findScriptView is what the provider's bar shows at the end: count is -1
// for an uncounted match. Error is whether the bar shows an error, Cleanup
// whether the pane shows a find cleanup error.
type findScriptView struct {
	Count   int  `json:"count"`
	Current int  `json:"current"`
	Pending bool `json:"pending"`
	Error   bool `json:"error"`
	Cleanup bool `json:"cleanup"`
}

type findScript struct {
	Name  string           `json:"name"`
	Mount string           `json:"mount"`
	Page  string           `json:"page"`
	Steps []findScriptStep `json:"steps"`
	// Bar is whether the provider's bar is open at the end, and View what
	// it shows: the real provider must leave both so.
	Bar  bool           `json:"bar"`
	View findScriptView `json:"view"`
}

// scripts are the providers' recorded views of the sequence.
func (e *findTableEnv) scripts(t *testing.T, name string) []findScript {
	t.Helper()
	var out []findScript
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
		if len(p.steps) == 0 {
			continue
		}
		out = append(out, findScript{
			Name: name + " / " + label, Mount: e.labels[p.mount], Page: "page-1", Steps: p.steps, Bar: p.open,
			View: findScriptView{Count: p.count, Current: p.current, Pending: p.pending, Error: p.errored, Cleanup: p.cleanup},
		})
	}
	return out
}

// checkFindScripts compares the scripts with the copy the frontend replays,
// or rewrites it with AO_UPDATE_FIND_SCRIPTS=1.
func checkFindScripts(t *testing.T, file string, scripts []findScript) {
	t.Helper()
	// Sorted first, so the name kept for scripts alike does not depend on
	// the order the sequences ran in.
	sort.SliceStable(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	seen := map[string]bool{}
	var unique []findScript
	for _, script := range scripts {
		key, err := json.Marshal(struct {
			Mount string
			Steps []findScriptStep
			Bar   bool
		}{script.Mount, script.Steps, script.Bar})
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		unique = append(unique, script)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].Name < unique[j].Name })
	encoded, err := json.MarshalIndent(unique, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	path := findScriptsDir + file
	if updateFindScripts {
		if err := os.MkdirAll(findScriptsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if t.Failed() {
		return
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the frontend's find scripts: %v (regenerate with AO_UPDATE_FIND_SCRIPTS=1)", err)
	}
	if !bytes.Equal(current, encoded) {
		t.Fatalf("%s is out of date with this table: regenerate with AO_UPDATE_FIND_SCRIPTS=1", path)
	}
}
