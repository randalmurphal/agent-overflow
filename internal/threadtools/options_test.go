package threadtools

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func catalogOf(name string) Catalog {
	return Catalog{
		Reachable: true,
		OS:        "darwin",
		Providers: []ProviderOption{{ID: "claude", Name: "Claude Code", DefaultModel: "opus-5", Models: []ModelOption{{Slug: "opus-5", Efforts: []string{"low", "high"}, DefaultEffort: "high"}}}},
		Projects:  []ProjectOption{{ID: "p1", Name: name + " repo", Path: "/src/" + name, Workspaces: []WorkspaceOption{{Path: "/src/" + name}}}},
	}
}

// TestOptionsWithoutPairingIsOneInlineRow that says nothing about
// computers.
func TestOptionsWithoutPairingIsOneInlineRow(t *testing.T) {
	app := newFakeApp("Laptop")
	app.catalog = catalogOf("laptop")
	app.addThread(Thread{ID: "caller-thread", Provider: "codex", Model: "gpt-5", Effort: "high", Mode: "plan", RuntimeMode: string(provider.RuntimeApprovalRequired)})

	result := call(t, New(app), localCaller(), "thread_options", `{}`)
	if _, present := result["computers"]; present {
		t.Fatal("an unpaired result groups by computer")
	}
	if result["os"] != "darwin" || result["reachable"] != true {
		t.Fatalf("result = %v", result)
	}
	if len(rows(t, result["providers"])) != 1 || len(rows(t, result["projects"])) != 1 {
		t.Fatalf("catalogs did not come through: %v", result)
	}
	modes := rows(t, result["runtime_modes"])
	if len(modes) != len(provider.AllRuntimeModes) {
		t.Fatalf("runtime_modes = %v", result["runtime_modes"])
	}
	defaults := result["defaults"].(map[string]any)
	if defaults["provider"] != "codex" || defaults["model"] != "gpt-5" || defaults["effort"] != "high" || defaults["mode"] != "plan" || defaults["runtime_mode"] != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("defaults are not the caller's own settings: %v", defaults)
	}
	if _, present := result["computer_id"]; present {
		t.Error("an unpaired row names a computer")
	}
}

// TestOptionsPutsTheCallersComputerFirstAndCarriesDefaultsOnlyThere.
func TestOptionsPutsTheCallersComputerFirstAndCarriesDefaultsOnlyThere(t *testing.T) {
	p := newPair(t)
	p.local.catalog, p.remote.catalog = catalogOf("laptop"), catalogOf("studio")
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})
	p.remote.addThread(Thread{ID: "caller-thread", Provider: "codex", Model: "gpt-5"})

	result := call(t, p.server, localCaller(), "thread_options", `{}`)
	computers := rows(t, result["computers"])
	if len(computers) != 2 {
		t.Fatalf("computers = %v", result["computers"])
	}
	first, second := computers[0].(map[string]any), computers[1].(map[string]any)
	if first["computer_id"] != "laptop" || first["local"] != true {
		t.Fatalf("the caller's own computer is not first: %v", first)
	}
	if first["defaults"] == nil {
		t.Error("the caller's row does not state the defaults a spawn inherits")
	}
	if second["computer_id"] != "studio" || second["defaults"] != nil {
		t.Fatalf("a peer row carries the wrong identity or someone else's defaults: %v", second)
	}
	if projects := rows(t, second["projects"]); len(projects) != 1 || field(t, projects[0], "project") != "studio repo" {
		t.Fatalf("the peer's own projects did not come back: %v", second["projects"])
	}
}

// TestOptionsTurnsASilentComputerIntoAnErrorRow.
func TestOptionsTurnsASilentComputerIntoAnErrorRow(t *testing.T) {
	p := newPair(t)
	p.local.catalog = catalogOf("laptop")
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude"})
	p.local.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}

	result := call(t, p.server, localCaller(), "thread_options", `{}`)
	if len(rows(t, result["computers"])) != 1 {
		t.Fatalf("computers = %v", result["computers"])
	}
	if len(rows(t, result["errors"])) != 1 {
		t.Fatalf("errors = %v", result["errors"])
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "did not answer") {
		t.Errorf("note = %q", note)
	}
}

// TestOptionsForOneComputerAsksOnlyThatOne.
func TestOptionsForOneComputerAsksOnlyThatOne(t *testing.T) {
	p := newPair(t)
	p.remote.catalog = catalogOf("studio")
	p.remote.addThread(Thread{ID: "caller-thread", Provider: "codex"})

	result := call(t, p.server, localCaller(), "thread_options", `{"computer_id":"studio"}`)
	computers := rows(t, result["computers"])
	if len(computers) != 1 || field(t, computers[0], "computer_id") != "studio" {
		t.Fatalf("computers = %v", result["computers"])
	}
	callErr(t, p.server, localCaller(), "thread_options", `{"computer_id":"nowhere"}`, CodeInvalidRequest)
}

// TestRuntimeModeOptionsExplainEachMode, because a permission level the
// model cannot read is a permission level it will guess at.
func TestRuntimeModeOptionsExplainEachMode(t *testing.T) {
	options := RuntimeModeOptions()
	if len(options) != len(provider.AllRuntimeModes) {
		t.Fatalf("%d options for %d modes", len(options), len(provider.AllRuntimeModes))
	}
	for index, mode := range provider.AllRuntimeModes {
		option := options[index]
		if option["runtime_mode"] != string(mode) {
			t.Fatalf("option %d = %v, want %s", index, option, mode)
		}
		if text, _ := option["meaning"].(string); text == "" {
			t.Errorf("%s has no explanation", mode)
		}
	}
}

// TestOptionsWithoutPairingReturnsItsOwnFailure. The one computer a solo
// call describes is the computer the caller is running on, so answering
// {"reachable": false} for it would describe the caller as unreachable
// instead of saying the read failed.
func TestOptionsWithoutPairingReturnsItsOwnFailure(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: "caller-thread"})
	app.catalogErr = publicf(CodeInvalidRequest, "The project list could not be read.")

	message := callErr(t, New(app), localCaller(), "thread_options", `{}`, CodeInvalidRequest)
	if !strings.Contains(message, "project list could not be read") {
		t.Errorf("message = %q", message)
	}
}
