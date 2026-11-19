package threadtools

import (
	"context"
	"encoding/json"
	"errors"
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
			{Name: "Haiku", UseFor: "Mechanical work.", Provider: "claude", Model: "haiku-4"},
			{Name: "Hasty", UseFor: "Never listed: its model has no effort.", Provider: "claude", Model: "haiku-4", Effort: "high"},
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

func profileNames(listed []ProfileOption) []string {
	names := make([]string, 0, len(listed))
	for _, option := range listed {
		names = append(names, option.Name)
	}
	return names
}

// A profile is listed where its provider and model are offered and its
// effort fits its model: a model the computer lacks leaves it out, and so
// does an effort on a model with none. An empty setting inherits the
// spawner's, an effort only where the model offers it.
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
	if haiku := byName["Haiku"]; haiku.Model != "haiku-4" || haiku.Effort != "" {
		t.Errorf("Haiku resolves to %+v, want its model with no effort", haiku)
	}
	if mine := byName["Mine"]; mine.Provider != "claude" || mine.Model != "opus-5" || mine.Effort != "medium" || mine.RuntimeMode != "read-only" {
		t.Errorf("an inheriting profile resolves to %+v, want the spawner's settings with its own mode", mine)
	}
	if sol := byName["Sol"]; !sol.Instructions || sol.Effort != "high" {
		t.Errorf("Sol resolves to %+v", sol)
	}
	// A profile naming the spawner's model and no effort takes the
	// spawner's effort, and the model's default where the model lacks it.
	roster := Roster{Profiles: []AgentProfile{{Name: "Opus", UseFor: "x", Provider: "claude", Model: "opus-5"}}}
	for spawnerEffort, want := range map[string]string{"medium": "medium", "max": "xhigh", "": "xhigh"} {
		listed := ListProfiles(roster, []ProviderOption{claudeOption}, SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: spawnerEffort})
		if len(listed) != 1 || listed[0].Effort != want {
			t.Errorf("spawner effort %q: listed %+v, want effort %q", spawnerEffort, listed, want)
		}
	}
}

// The guide names, in roster order, every profile whose provider is
// offered, whatever its model: Astra's model is in no catalog here and
// Hasty's effort does not fit its model, and both are named. A profile on a
// provider not offered is not, and one that inherits its provider follows
// the spawner's.
func TestGuideProfileNamesFollowTheOfferedProviders(t *testing.T) {
	cases := []struct {
		name    string
		offered []string
		spawner string
		want    []string
	}{
		{"both", []string{"claude", "codex"}, "claude", []string{"Opus", "Haiku", "Hasty", "Sol", "Astra", "Mine"}},
		{"both, codex spawner", []string{"claude", "codex"}, "codex", []string{"Opus", "Haiku", "Hasty", "Sol", "Astra", "Mine"}},
		{"claude only", []string{"claude"}, "claude", []string{"Opus", "Haiku", "Hasty", "Mine"}},
		{"claude only, codex spawner", []string{"claude"}, "codex", []string{"Opus", "Haiku", "Hasty"}},
		{"codex only", []string{"codex"}, "codex", []string{"Sol", "Astra", "Mine"}},
		{"nothing offered", nil, "claude", []string{}},
	}
	for _, tc := range cases {
		if got := GuideProfileNames(testRoster(), tc.offered, tc.spawner); !slices.Equal(got, tc.want) {
			t.Errorf("%s: named %v, want %v", tc.name, got, tc.want)
		}
	}
}

// One resolution answers every spawn: what the choice names over the
// spawner's settings, inheriting only on the spawner's provider, and a
// refusal that says whether the computer lacks something or the choice
// does not fit its model.
func TestResolveSpawn(t *testing.T) {
	claude := claudeOption
	claude.Unlisted = []ModelOption{{Slug: "opus-4", Efforts: []string{"low"}, DefaultEffort: "low"}}
	catalog := []ProviderOption{claude, codexOption}
	spawner := SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: "medium"}
	cases := []struct {
		name    string
		spawner SpawnDefaults
		choice  SpawnChoice
		want    SpawnChoice
		refusal string
		missing bool
	}{
		{"all inherited", spawner, SpawnChoice{}, SpawnChoice{"claude", "opus-5", "medium"}, "", false},
		{"explicit effort", spawner, SpawnChoice{Effort: "low"}, SpawnChoice{"claude", "opus-5", "low"}, "", false},
		{"explicit model, inherited effort", spawner, SpawnChoice{Model: "opus-5"}, SpawnChoice{"claude", "opus-5", "medium"}, "", false},
		{"inherited effort the model lacks", SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: "max"}, SpawnChoice{}, SpawnChoice{"claude", "opus-5", "xhigh"}, "", false},
		{"a model with no effort drops an inherited one", spawner, SpawnChoice{Model: "haiku-4"}, SpawnChoice{"claude", "haiku-4", ""}, "", false},
		{"another provider inherits nothing", spawner, SpawnChoice{Provider: "codex"}, SpawnChoice{"codex", "sol", "low"}, "", false},
		{"an unlisted model resolves", spawner, SpawnChoice{Model: "opus-4"}, SpawnChoice{"claude", "opus-4", "low"}, "", false},
		{"an effort on a model with none", spawner, SpawnChoice{Model: "haiku-4", Effort: "high"}, SpawnChoice{}, "Model haiku-4 has no reasoning effort setting on here.", false},
		{"an effort the model lacks", spawner, SpawnChoice{Effort: "max"}, SpawnChoice{}, `Model opus-5 does not offer effort "max" on here. It offers: low, medium, high, xhigh.`, false},
		{"a model the provider lacks", spawner, SpawnChoice{Model: "opus-9"}, SpawnChoice{}, `Claude Code does not offer model "opus-9" on here. It offers: opus-5, haiku-4.`, true},
		{"a provider not offered", spawner, SpawnChoice{Provider: "gemini"}, SpawnChoice{}, "gemini does not run on here.", true},
	}
	for _, tc := range cases {
		got, err := ResolveSpawn(tc.spawner, tc.choice, catalog, "here")
		if tc.refusal == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
			}
			continue
		}
		var refusal *SpawnRefusal
		if !errors.As(err, &refusal) || refusal.Reason != tc.refusal || refusal.Missing != tc.missing {
			t.Errorf("%s: %v (%+v); want %q, missing %v", tc.name, err, refusal, tc.refusal, tc.missing)
		}
	}
}

// The settings editor refuses a profile whose effort does not fit its
// model here and only notes one this computer lacks the provider or model
// for; a profile that inherits its model is checked against each spawner.
func TestProfileProblem(t *testing.T) {
	catalog := []ProviderOption{claudeOption}
	cases := []struct {
		profile          AgentProfile
		problem, missing string
	}{
		{AgentProfile{Provider: "claude", Model: "opus-5", Effort: "high"}, "", ""},
		{AgentProfile{Provider: "claude", Model: "haiku-4", Effort: "high"}, "Model haiku-4 has no reasoning effort setting on this computer.", ""},
		{AgentProfile{Provider: "claude", Model: "opus-5", Effort: "max"}, `Model opus-5 does not offer effort "max" on this computer. It offers: low, medium, high, xhigh.`, ""},
		{AgentProfile{Provider: "claude", Model: "opus-9"}, "", `Claude Code does not offer model "opus-9" on this computer. It offers: opus-5, haiku-4.`},
		{AgentProfile{Provider: "codex", Model: "sol", Effort: "high"}, "", "Codex does not run on this computer."},
		{AgentProfile{Provider: "codex"}, "", "Codex does not run on this computer."},
		{AgentProfile{Provider: "claude", Effort: "max"}, "", ""},
		{AgentProfile{Effort: "max"}, "", ""},
	}
	for _, tc := range cases {
		problem, missing := ProfileProblem(tc.profile, catalog)
		if problem != tc.problem || missing != tc.missing {
			t.Errorf("%+v: problem %q, missing %q; want %q, %q", tc.profile, problem, missing, tc.problem, tc.missing)
		}
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

	reply := call(t, server, localCaller(), "thread_spawn", `{"prompt":"review","profile":"sol","instructions":"Focus on the parser."}`)
	if reply["instructions"] != "Look for bugs.\n\nFocus on the parser." {
		t.Fatalf("the spawn's reply records instructions %q", reply["instructions"])
	}
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
	if !strings.Contains(message, `"Sol" cannot run: Codex does not run on this computer.`) {
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
	if !strings.Contains(message, `"Opus" cannot run: Claude Code does not run on Studio.`) {
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
