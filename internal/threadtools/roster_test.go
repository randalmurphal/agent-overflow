package threadtools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// olderPeer answers thread_options as a computer that predates spawn
// instructions does: without the field that says it adds them.
type olderPeer struct{ *serverPeer }

func (p olderPeer) Query(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	raw, err := p.serverPeer.Query(ctx, name, args)
	if err != nil || name != "thread_options" {
		return raw, err
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	delete(answer, "spawn_instructions")
	delete(answer, "profiles")
	return json.Marshal(answer)
}

var (
	claudeOption = ProviderOption{ID: "claude", Name: "Claude Code", DefaultModel: "opus-5", Models: []ModelOption{
		{Slug: "opus-5", Efforts: []string{"low", "medium", "high", "xhigh"}, DefaultEffort: "xhigh"},
		{Slug: "haiku-4", Efforts: nil},
	}}
	codexOption = ProviderOption{ID: "codex", Name: "Codex", DefaultModel: "sol", Models: []ModelOption{
		{Slug: "sol", Efforts: []string{"low", "high"}, DefaultEffort: "low"},
	}}
)

func testRoster() Roster {
	return Roster{
		Profiles: []AgentProfile{
			{Name: "Opus", UseFor: "Implementation.", Provider: "claude", Model: "opus-5", Effort: "high"},
			{Name: "Haiku", UseFor: "Mechanical work.", Provider: "claude", Model: "haiku-4", Effort: "high"},
			{Name: "Sol", UseFor: "Review.", Provider: "codex", Model: "sol", Effort: "high", Instructions: "Look for bugs."},
			{Name: "Astra", UseFor: "Architecture.", Provider: "codex", Model: "astra"},
			{Name: "Mine", UseFor: "Like me, read-only.", RuntimeMode: "read-only"},
		},
		Notes: []AgentNote{
			{Text: "always"},
			{Text: "council of both", When: NoteBothProviders},
			{Text: "council of one", When: NoteOneProvider},
		},
	}
}

func profileNames(listed []ProfileOption) []string { return ProfileNames(listed) }

// A profile is listed where its provider and model are offered: a model the
// computer lacks leaves it out, a model with no effort drops the effort, and
// an empty setting inherits the spawner's.
func TestListProfilesKeepsWhatTheComputerCanRun(t *testing.T) {
	defaults := SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: "medium"}
	cases := []struct {
		name      string
		providers []ProviderOption
		want      []string
	}{
		{"both", []ProviderOption{claudeOption, codexOption}, []string{"Opus", "Haiku", "Sol", "Mine"}},
		{"claude only", []ProviderOption{claudeOption}, []string{"Opus", "Haiku", "Mine"}},
		{"codex only", []ProviderOption{codexOption}, []string{"Sol"}},
		{"none", nil, []string{}},
	}
	for _, tc := range cases {
		listed := ListProfiles(testRoster(), tc.providers, defaults)
		if got := profileNames(listed); !slices.Equal(got, tc.want) {
			t.Errorf("%s: listed %v, want %v", tc.name, got, tc.want)
		}
	}
	listed := ListProfiles(testRoster(), []ProviderOption{claudeOption, codexOption}, defaults)
	byName := map[string]ProfileOption{}
	for _, option := range listed {
		byName[option.Name] = option
	}
	if haiku := byName["Haiku"]; haiku.Effort != "" {
		t.Errorf("Haiku lists effort %q for a model with none", haiku.Effort)
	}
	if mine := byName["Mine"]; mine.Provider != "claude" || mine.Model != "opus-5" || mine.Effort != "medium" || mine.RuntimeMode != "read-only" {
		t.Errorf("an inheriting profile resolves to %+v, want the spawner's settings with its own mode", mine)
	}
	if sol := byName["Sol"]; !sol.Instructions || sol.Effort != "high" {
		t.Errorf("Sol resolves to %+v", sol)
	}
}

// A note for both providers shows only when the listed profiles run on
// both; one for a single provider only when they all run on one and there
// are two or more to choose between.
func TestApplicableNotesFollowTheProvidersListed(t *testing.T) {
	defaults := SpawnDefaults{Provider: "claude", Model: "opus-5"}
	both := ListProfiles(testRoster(), []ProviderOption{claudeOption, codexOption}, defaults)
	if got := ApplicableNotes(testRoster().Notes, both); !slices.Equal(got, []string{"always", "council of both"}) {
		t.Errorf("both providers: notes %v", got)
	}
	claude := ListProfiles(testRoster(), []ProviderOption{claudeOption}, defaults)
	if got := ApplicableNotes(testRoster().Notes, claude); !slices.Equal(got, []string{"always", "council of one"}) {
		t.Errorf("Claude only: notes %v", got)
	}
	codex := ListProfiles(testRoster(), []ProviderOption{codexOption}, SpawnDefaults{Provider: "codex"})
	if got := ApplicableNotes(testRoster().Notes, codex); !slices.Equal(got, []string{"always"}) {
		t.Errorf("one Codex profile: notes %v, want no council of one model", got)
	}
	if got := ApplicableNotes(testRoster().Notes, nil); got != nil {
		t.Errorf("no profiles: notes %v", got)
	}
}

// thread_options lists the profiles this computer can run, with the notes
// that apply to them.
func TestOptionsListsTheProfilesAndNotes(t *testing.T) {
	app := newFakeApp("Laptop")
	app.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption}}
	app.roster = testRoster()
	app.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5", Effort: "high"})

	result := call(t, New(app), localCaller(), "thread_options", `{}`)
	var names []string
	for _, row := range rows(t, result["profiles"]) {
		names = append(names, field(t, row, "name").(string))
		if field(t, row, "use_for") == "" {
			t.Errorf("profile %v has no use_for line", row)
		}
	}
	if !slices.Equal(names, []string{"Opus", "Haiku", "Mine"}) {
		t.Fatalf("profiles = %v", names)
	}
	notes := rows(t, result["profile_notes"])
	if len(notes) != 2 || notes[1] != "council of one" {
		t.Fatalf("profile_notes = %v", notes)
	}
	result = call(t, New(app), localCaller(), "thread_options", `{"what":"projects"}`)
	if _, present := result["profiles"]; present {
		t.Error("a projects answer lists profiles")
	}
}

// A paired computer is answered with the caller's roster resolved against
// what that computer offers, whatever roster it keeps itself: a profile
// this computer cannot run is listed there when that computer offers its
// provider, and the notes cover both.
func TestOptionsResolvesTheCallersRosterOnAPairedComputer(t *testing.T) {
	p := newPair(t)
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption}}
	p.remote.catalog = Catalog{Reachable: true, Providers: []ProviderOption{codexOption}}
	p.local.roster = testRoster()
	p.remote.roster = Roster{Profiles: []AgentProfile{{Name: "Theirs", UseFor: "x", Provider: "codex", Model: "sol"}}}
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})

	result := call(t, p.server, localCaller(), "thread_options", `{}`)
	computers := rows(t, result["computers"])
	var studio []string
	for _, row := range rows(t, field(t, computers[1], "profiles")) {
		studio = append(studio, field(t, row, "name").(string))
	}
	if !slices.Equal(studio, []string{"Sol"}) {
		t.Fatalf("the paired computer lists %v, want the caller's Sol only", studio)
	}
	notes := rows(t, result["profile_notes"])
	if !slices.Contains(notes, any("council of both")) {
		t.Fatalf("profile_notes = %v, want the note for both providers", notes)
	}
}

// A spawn by profile takes the profile's settings and instructions, and
// what the call names itself wins; a provider other than the profile's
// takes neither its model nor its effort.
func TestSpawnByProfileTakesItsSettingsWithOverrides(t *testing.T) {
	app := newFakeApp("Laptop")
	app.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption, codexOption}}
	app.roster = testRoster()
	app.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})
	server := New(app)

	call(t, server, localCaller(), "thread_spawn", `{"prompt":"review","profile":"sol","instructions":"Focus on the parser."}`)
	got := app.spawns[0]
	if got.Profile != "Sol" || got.Provider != "codex" || got.Model != "sol" || got.Effort != "high" {
		t.Fatalf("spawn = %+v, want Sol's settings", got)
	}
	if got.Instructions != "Look for bugs.\n\nFocus on the parser." {
		t.Fatalf("instructions = %q, want the profile's then the call's", got.Instructions)
	}

	call(t, server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Opus","effort":"low","runtime_mode":"read-only"}`)
	if got := app.spawns[1]; got.Model != "opus-5" || got.Effort != "low" || got.RuntimeMode != "read-only" {
		t.Fatalf("spawn = %+v, want the call's effort and mode over the profile's", got)
	}

	call(t, server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Opus","provider":"codex"}`)
	if got := app.spawns[2]; got.Provider != "codex" || got.Model != "" || got.Effort != "" {
		t.Fatalf("spawn = %+v, want codex with neither Opus's model nor its effort", got)
	}
}

// A profile that is not in the roster, or that the target computer cannot
// run, is refused naming it, and nothing is spawned.
func TestSpawnByProfileRefusesWhatCannotRun(t *testing.T) {
	app := newFakeApp("Laptop")
	app.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption}}
	app.roster = testRoster()
	app.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})
	server := New(app)

	message := callErr(t, server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Nobody"}`, CodeInvalidRequest)
	if !strings.Contains(message, `"Nobody"`) || !strings.Contains(message, "Opus") {
		t.Errorf("unknown profile refusal = %q", message)
	}
	message = callErr(t, server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Sol"}`, CodeInvalidRequest)
	if !strings.Contains(message, `"Sol" cannot run on this computer`) || !strings.Contains(message, "Codex") {
		t.Errorf("unoffered profile refusal = %q", message)
	}
	long := strings.Repeat("i", MaxSpawnInstructionsBytes)
	message = callErr(t, server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Sol","instructions":"`+long+`"}`, CodeInvalidRequest)
	if !strings.Contains(message, "together") {
		t.Errorf("over-long combined instructions refusal = %q", message)
	}
	if len(app.spawns) != 0 {
		t.Fatalf("refused spawns reached the app: %v", app.spawns)
	}
}

// A spawn to a paired computer resolves the profile against what that
// computer offers: one this computer lacks the provider for runs there.
func TestSpawnByProfileRunsOnAPairedComputerThatOffersIt(t *testing.T) {
	p := newPair(t)
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption}}
	p.remote.catalog = Catalog{Reachable: true, Providers: []ProviderOption{codexOption}}
	p.local.roster = testRoster()
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})

	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"review","profile":"Sol","computer_id":"studio","project_id":"p1"}`)
	if got := p.local.spawns[0]; got.ComputerID != "studio" || got.Provider != "codex" || got.Model != "sol" || got.Instructions != "Look for bugs." {
		t.Fatalf("spawn = %+v", got)
	}
	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Opus","computer_id":"studio","project_id":"p1"}`, CodeInvalidRequest)
	if !strings.Contains(message, "cannot run on Studio") {
		t.Errorf("refusal = %q", message)
	}
}

// A computer that predates spawn instructions would drop them silently, so
// a spawn there that carries any is refused.
func TestSpawnInstructionsAreRefusedForAComputerThatPredatesThem(t *testing.T) {
	p := newPair(t)
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{claudeOption}}
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "opus-5"})
	p.local.peers["studio"] = olderPeer{p.peer}

	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","instructions":"Be terse.","computer_id":"studio","project_id":"p1"}`, CodeInvalidRequest)
	if !strings.Contains(message, "older version") {
		t.Errorf("refusal = %q", message)
	}
	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","computer_id":"studio","project_id":"p1"}`)
	if len(p.local.spawns) != 1 {
		t.Fatalf("a spawn without instructions did not go through: %v", p.local.spawns)
	}
}
