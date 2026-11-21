package threadtools

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// model reads text as providerName's grammar does, as the roster does.
func model(providerName, text string) provider.ModelRef {
	return provider.ParseModelRef(providerName, text)
}

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
			{Name: "Opus", UseFor: "Implementation.", Provider: "claude", Model: model("claude", "opus-5"), Effort: "high"},
			{Name: "Haiku", UseFor: "Mechanical work.", Provider: "claude", Model: model("claude", "haiku-4")},
			{Name: "Hasty", UseFor: "Never listed: its model has no effort.", Provider: "claude", Model: model("claude", "haiku-4"), Effort: "high"},
			{Name: "Sol", UseFor: "Review.", Provider: "codex", Model: model("codex", "sol"), Effort: "high", Instructions: "Look for bugs."},
			{Name: "Astra", UseFor: "Architecture.", Provider: "codex", Model: model("codex", "astra")},
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
	roster := Roster{Profiles: []AgentProfile{{Name: "Opus", UseFor: "x", Provider: "claude", Model: model("claude", "opus-5")}}}
	for spawnerEffort, want := range map[string]string{"medium": "medium", "max": "xhigh", "": "xhigh"} {
		listed := ListProfiles(roster, []ProviderOption{claudeOption}, SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: spawnerEffort})
		if len(listed) != 1 || listed[0].Effort != want {
			t.Errorf("spawner effort %q: listed %+v, want effort %q", spawnerEffort, listed, want)
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
		want    ResolvedSpawn
		refusal string
		missing bool
	}{
		{"all inherited", spawner, SpawnChoice{}, ResolvedSpawn{"claude", "opus-5", "medium"}, "", false},
		{"explicit effort", spawner, SpawnChoice{Effort: "low"}, ResolvedSpawn{"claude", "opus-5", "low"}, "", false},
		{"explicit model, inherited effort", spawner, SpawnChoice{Model: model("claude", "opus-5")}, ResolvedSpawn{"claude", "opus-5", "medium"}, "", false},
		{"inherited effort the model lacks", SpawnDefaults{Provider: "claude", Model: "opus-5", Effort: "max"}, SpawnChoice{}, ResolvedSpawn{"claude", "opus-5", "xhigh"}, "", false},
		{"a model with no effort drops an inherited one", spawner, SpawnChoice{Model: model("claude", "haiku-4")}, ResolvedSpawn{"claude", "haiku-4", ""}, "", false},
		{"another provider inherits nothing", spawner, SpawnChoice{Provider: "codex"}, ResolvedSpawn{"codex", "sol", "low"}, "", false},
		{"an unlisted model resolves", spawner, SpawnChoice{Model: model("claude", "opus-4")}, ResolvedSpawn{"claude", "opus-4", "low"}, "", false},
		{"an effort on a model with none", spawner, SpawnChoice{Model: model("claude", "haiku-4"), Effort: "high"}, ResolvedSpawn{}, "Model haiku-4 has no reasoning effort setting on here.", false},
		{"an effort the model lacks", spawner, SpawnChoice{Effort: "max"}, ResolvedSpawn{}, `Model opus-5 does not offer effort "max" on here. It offers: low, medium, high, xhigh.`, false},
		{"a model the provider lacks", spawner, SpawnChoice{Model: model("claude", "opus-9")}, ResolvedSpawn{}, `Claude Code does not offer model "opus-9" on here. It offers: opus-5, haiku-4.`, true},
		{"a provider not offered", spawner, SpawnChoice{Provider: "gemini"}, ResolvedSpawn{}, "gemini does not run on here.", true},
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
		{AgentProfile{Provider: "claude", Model: model("claude", "opus-5"), Effort: "high"}, "", ""},
		{AgentProfile{Provider: "claude", Model: model("claude", "haiku-4"), Effort: "high"}, "Model haiku-4 has no reasoning effort setting on this computer.", ""},
		{AgentProfile{Provider: "claude", Model: model("claude", "opus-5"), Effort: "max"}, `Model opus-5 does not offer effort "max" on this computer. It offers: low, medium, high, xhigh.`, ""},
		{AgentProfile{Provider: "claude", Model: model("claude", "opus-9")}, "", `Claude Code does not offer model "opus-9" on this computer. It offers: opus-5, haiku-4.`},
		{AgentProfile{Provider: "codex", Model: model("codex", "sol"), Effort: "high"}, "", "Codex does not run on this computer."},
		{AgentProfile{Provider: "codex"}, "", "Codex does not run on this computer."},
		{AgentProfile{Provider: "claude", Effort: "max"}, "", ""},
		{AgentProfile{Effort: "max"}, "", ""},
	}
	for _, tc := range cases {
		check := ProfileProblem(tc.profile, catalog)
		if check.Problem != tc.problem || check.Missing != tc.missing {
			t.Errorf("%+v: problem %q, missing %q; want %q, %q", tc.profile, check.Problem, check.Missing, tc.problem, tc.missing)
		}
	}
}

// liveClaude and liveCodex are the model lists this computer's thread tools
// reported on 2026-11-21, in their order: Claude's probe answer with Opus
// 5 still listed because a thread ran it that week, and Codex's live list.
var (
	liveClaude = ProviderOption{ID: "claude", Name: "Claude Code", DefaultModel: "claude-opus-5-5", Models: []ModelOption{
		{Slug: "claude-opus-5-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "xhigh"},
		{Slug: "claude-fable-5-2", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "xhigh"},
		{Slug: "claude-sonnet-5-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high"},
		{Slug: "claude-haiku-5-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
		{Slug: "claude-opus-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "xhigh"},
	}, Unlisted: []ModelOption{
		{Slug: "claude-fable-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "xhigh"},
		{Slug: "claude-sonnet-5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high"},
		{Slug: "claude-opus-4-8", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "xhigh"},
	}}
	liveCodex = ProviderOption{ID: "codex", Name: "Codex", DefaultModel: "gpt-6.2-sol", Models: []ModelOption{
		{Slug: "gpt-6.2-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
		{Slug: "gpt-6.2-astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
		{Slug: "gpt-6.1-terra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
		{Slug: "gpt-6.1-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
		{Slug: "gpt-6-astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
		{Slug: "gpt-6-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
		{Slug: "gpt-6-luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
		{Slug: "gpt-5.6-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
		{Slug: "gpt-5.6-terra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
		{Slug: "gpt-5.6-luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"},
	}}
)

// familyRoster is the seeded roster's shape: each profile names a family.
func familyRoster() Roster {
	profile := func(name, providerName, family string) AgentProfile {
		return AgentProfile{Name: name, UseFor: name + ".", Provider: providerName, Model: model(providerName, family), Effort: "high"}
	}
	return Roster{Profiles: []AgentProfile{
		profile("Luna", "codex", "gpt-luna"),
		profile("Terra", "codex", "gpt-terra"),
		profile("Sol", "codex", "gpt-sol"),
		profile("Astra", "codex", "gpt-astra"),
		profile("Haiku", "claude", "claude-haiku"),
		profile("Sonnet", "claude", "claude-sonnet"),
		profile("Opus", "claude", "claude-opus"),
		profile("Fable", "claude", "claude-fable"),
	}}
}

func listedModels(listed []ProfileOption) map[string]string {
	out := map[string]string{}
	for _, option := range listed {
		out[option.Name] = option.Model
	}
	return out
}

// A profile on a family runs the newest model of it this computer lists:
// on the lists this computer reported, every seeded family resolves to
// the newest it offers, not to a model it keeps only for threads already
// on it.
func TestFamilyProfilesRunTheNewestListedModel(t *testing.T) {
	got := listedModels(ListProfiles(familyRoster(), []ProviderOption{liveClaude, liveCodex}, SpawnDefaults{Provider: "claude", Model: "claude-fable-5-2"}))
	want := map[string]string{
		"Opus": "claude-opus-5-5", "Fable": "claude-fable-5-2", "Sonnet": "claude-sonnet-5-5", "Haiku": "claude-haiku-5-5",
		"Sol": "gpt-6.2-sol", "Astra": "gpt-6.2-astra", "Terra": "gpt-6.1-terra", "Luna": "gpt-6-luna",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("family profiles run %v, want %v", got, want)
	}
}

// A newer model appearing in a computer's list moves a family profile with
// no change to the roster, and an exact profile stays on its model.
func TestANewerListedModelMovesAFamilyProfileOnly(t *testing.T) {
	roster := Roster{Profiles: []AgentProfile{
		{Name: "Opus", UseFor: "x", Provider: "claude", Model: model("claude", "claude-opus"), Effort: "high"},
		{Name: "Pinned", UseFor: "x", Provider: "claude", Model: model("claude", "claude-opus-5-5"), Effort: "high"},
		{Name: "Older", UseFor: "x", Provider: "claude", Model: model("claude", "claude-opus-5"), Effort: "high"},
	}}
	defaults := SpawnDefaults{Provider: "claude"}
	before := listedModels(ListProfiles(roster, []ProviderOption{liveClaude}, defaults))
	newer := liveClaude
	newer.Models = append([]ModelOption{{Slug: "claude-opus-5-10", Efforts: []string{"high"}, DefaultEffort: "high"}}, liveClaude.Models...)
	after := listedModels(ListProfiles(roster, []ProviderOption{newer}, defaults))
	if before["Opus"] != "claude-opus-5-5" || after["Opus"] != "claude-opus-5-10" {
		t.Errorf("Opus ran %q, then %q once 5.10 was listed; want claude-opus-5-5 then claude-opus-5-10", before["Opus"], after["Opus"])
	}
	for _, name := range []string{"Pinned", "Older"} {
		if before[name] != after[name] || after[name] == "claude-opus-5-10" {
			t.Errorf("exact profile %s moved from %q to %q", name, before[name], after[name])
		}
	}
}

// A family with no listed member on a computer leaves the profile out of
// what that computer lists, even when it keeps an older member for
// threads already on it, and the settings check says why.
func TestAFamilyWithNoListedMemberIsNotListed(t *testing.T) {
	roster := Roster{Profiles: []AgentProfile{
		{Name: "Opus", UseFor: "x", Provider: "claude", Model: model("claude", "claude-opus")},
		{Name: "Haiku", UseFor: "x", Provider: "claude", Model: model("claude", "claude-haiku")},
	}}
	older := ProviderOption{ID: "claude", DefaultModel: "claude-haiku-5-5", Models: []ModelOption{{Slug: "claude-haiku-5-5"}},
		Unlisted: []ModelOption{{Slug: "claude-opus-5"}}}
	if got := profileNames(ListProfiles(roster, []ProviderOption{older}, SpawnDefaults{Provider: "claude"})); !slices.Equal(got, []string{"Haiku"}) {
		t.Fatalf("listed %v, want Haiku only", got)
	}
	check := ProfileProblem(roster.Profiles[0], []ProviderOption{older})
	if check.Missing != "Claude Code offers no claude-opus model on this computer. It offers: claude-haiku-5-5." || check.Runs != "" {
		t.Errorf("check = %+v", check)
	}
}

// The settings check reports the model a profile runs here: the newest of
// a family, or the exact model; and for a family, the listed models its
// provider's rule cannot read.
func TestProfileProblemReportsWhatAProfileRuns(t *testing.T) {
	withPreview := liveClaude
	withPreview.Models = append(slices.Clone(liveClaude.Models), ModelOption{Slug: "claude-opus-6-preview", Efforts: []string{"high"}})
	family := ProfileProblem(AgentProfile{Provider: "claude", Model: model("claude", "claude-opus"), Effort: "high"}, []ProviderOption{withPreview})
	if family.Runs != "claude-opus-5-5" || !slices.Equal(family.Unreadable, []string{"claude-opus-6-preview"}) || family.Problem != "" || family.Missing != "" {
		t.Errorf("family check = %+v", family)
	}
	exact := ProfileProblem(AgentProfile{Provider: "claude", Model: model("claude", "claude-opus-5"), Effort: "high"}, []ProviderOption{withPreview})
	if exact.Runs != "claude-opus-5" || exact.Unreadable != nil {
		t.Errorf("exact check = %+v", exact)
	}
}

// A family's model is whatever this computer lists newest, so an effort
// that model does not take is a note that threads here are not offered the
// profile, as a missing model is, never a problem that holds the save. The
// same effort on the exact model is the person's choice and holds it.
func TestAFamilyEffortTheNewestModelLacksIsThisComputersLack(t *testing.T) {
	older := ProviderOption{ID: "claude", DefaultModel: "claude-opus-5", Models: []ModelOption{
		{Slug: "claude-opus-5", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
		{Slug: "claude-haiku-4-5"},
	}}
	const lack = "Model claude-haiku-4-5 has no reasoning effort setting on this computer."
	family := ProfileProblem(AgentProfile{Provider: "claude", Model: model("claude", "claude-haiku"), Effort: "high"}, []ProviderOption{older})
	if !reflect.DeepEqual(family, ProfileCheck{Missing: lack}) {
		t.Errorf("family check = %+v, want Missing %q", family, lack)
	}
	exact := ProfileProblem(AgentProfile{Provider: "claude", Model: model("claude", "claude-haiku-4-5"), Effort: "high"}, []ProviderOption{older})
	if !reflect.DeepEqual(exact, ProfileCheck{Problem: lack}) {
		t.Errorf("exact check = %+v, want Problem %q", exact, lack)
	}
	roster := Roster{Profiles: []AgentProfile{{Name: "Haiku", UseFor: "x", Provider: "claude", Model: model("claude", "claude-haiku"), Effort: "high"}}}
	if listed := ListProfiles(roster, []ProviderOption{older}, SpawnDefaults{Provider: "claude"}); len(listed) != 0 {
		t.Errorf("listed %+v, want the family profile left out", listed)
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
	p.remote.roster = Roster{Profiles: []AgentProfile{{Name: "Theirs", UseFor: "x", Provider: "codex", Model: model("codex", "sol")}}}
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

// A spawn to a paired computer by a family profile, or with a family as
// its model, carries the exact model that computer lists, whatever this
// computer lists: a computer of any release receives a model ID.
func TestAFamilySpawnToAPairedComputerCarriesAnExactModel(t *testing.T) {
	p := newPair(t)
	older := liveClaude
	older.DefaultModel, older.Models, older.Unlisted = "claude-opus-5", []ModelOption{liveClaude.Models[4], liveClaude.Models[3]}, nil
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{liveClaude}}
	p.remote.catalog = Catalog{Reachable: true, Providers: []ProviderOption{older}}
	p.local.roster = familyRoster()
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "claude-opus-5-5"})

	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Opus","computer_id":"studio","project_id":"p1"}`)
	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","model":"claude-haiku","computer_id":"studio","project_id":"p1"}`)
	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Opus"}`)
	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","model":"claude-opus-5"}`)
	got := []string{}
	for _, spawn := range p.local.spawns {
		got = append(got, spawn.Model)
	}
	if want := []string{"claude-opus-5", "claude-haiku-5-5", "claude-opus-5-5", "claude-opus-5"}; !slices.Equal(got, want) {
		t.Fatalf("spawns carried %v, want %v", got, want)
	}
}

// A family the target lists no member of refuses the spawn, naming the
// profile and the computer; with a family as the model and no profile it
// names the family and the computer. Nothing is spawned.
func TestAFamilySpawnTheTargetCannotRunIsRefused(t *testing.T) {
	p := newPair(t)
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{liveClaude}}
	p.remote.catalog = Catalog{Reachable: true, Providers: []ProviderOption{{ID: "claude", DefaultModel: "claude-haiku-5-5", Models: []ModelOption{{Slug: "claude-haiku-5-5"}}}}}
	p.local.roster = familyRoster()
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "claude-opus-5-5"})

	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","profile":"Fable","computer_id":"studio","project_id":"p1"}`, CodeInvalidRequest)
	if want := `Agent profile "Fable" cannot run: Claude Code offers no claude-fable model on Studio. It offers: claude-haiku-5-5.`; !strings.Contains(message, want) {
		t.Errorf("profile refusal = %q, want %q", message, want)
	}
	message = callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","model":"claude-fable","computer_id":"studio","project_id":"p1"}`, CodeInvalidRequest)
	if want := "Claude Code offers no claude-fable model on Studio."; !strings.Contains(message, want) {
		t.Errorf("model refusal = %q, want %q", message, want)
	}
	rows := rows(t, field(t, rows(t, call(t, p.server, localCaller(), "thread_options", `{"what":"models"}`)["computers"])[1], "profiles"))
	for _, row := range rows {
		if name := field(t, row, "name"); name != "Haiku" {
			t.Errorf("Studio lists profile %v, which it has no model for", name)
		}
	}
	if len(p.local.spawns) != 0 {
		t.Fatalf("refused spawns reached the app: %v", p.local.spawns)
	}
}

// A fork with a family as its model takes the exact model its computer
// lists, and is refused when that computer lists none.
func TestAForkWithAFamilyModelCarriesAnExactModel(t *testing.T) {
	p := newPair(t)
	p.local.catalog = Catalog{Reachable: true, Providers: []ProviderOption{liveClaude}}
	p.remote.catalog = Catalog{Reachable: true, Providers: []ProviderOption{liveClaude}}
	p.local.addThread(Thread{ID: "caller-thread", Provider: "claude", Model: "claude-opus-5-5"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Origin", Provider: "claude"})

	call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","from_thread":"`+remoteThreadID+`","model":"claude-sonnet"}`)
	if got := p.local.spawns[0]; got.Model != "claude-sonnet-5-5" || got.ComputerID != "studio" {
		t.Fatalf("fork = %+v, want claude-sonnet-5-5 on Studio", got)
	}
	message := callErr(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","from_thread":"`+remoteThreadID+`","model":"claude-poem"}`, CodeInvalidRequest)
	if !strings.Contains(message, "Claude Code offers no claude-poem model on Studio.") {
		t.Errorf("refusal = %q", message)
	}
}

// A fork runs on its source's provider, so the call's model is read by
// that provider's version rule, not the caller's: a Codex thread forking
// a Claude thread with claude-opus runs Claude's newest Opus, and the
// reverse runs Codex's newest Sol, here and on a paired computer.
func TestAForkAcrossProvidersReadsTheModelByTheSourcesProvider(t *testing.T) {
	cases := []struct {
		caller, source Thread
		model, want    string
	}{
		{Thread{ID: "caller-thread", Provider: "codex", Model: "gpt-6.2-sol"}, Thread{Provider: "claude", Model: "claude-opus-5"}, "claude-opus", "claude-opus-5-5"},
		{Thread{ID: "caller-thread", Provider: "claude", Model: "claude-opus-5-5"}, Thread{Provider: "codex", Model: "gpt-6-luna"}, "gpt-sol", "gpt-6.2-sol"},
	}
	for _, tc := range cases {
		p := newPair(t)
		both := Catalog{Reachable: true, Providers: []ProviderOption{liveClaude, liveCodex}}
		p.local.catalog, p.remote.catalog = both, both
		p.local.addThread(tc.caller)
		here, there := tc.source, tc.source
		here.ID, here.Title = localThreadID, "Here"
		there.ID, there.Title = remoteThreadID, "There"
		p.local.addThread(here)
		p.remote.addThread(there)

		for _, source := range []string{localThreadID, remoteThreadID} {
			call(t, p.server, localCaller(), "thread_spawn", `{"prompt":"x","from_thread":"`+source+`","model":"`+tc.model+`"}`)
			if got := p.local.spawns[len(p.local.spawns)-1]; got.Model != tc.want {
				t.Errorf("%s caller forking a %s thread (%s) with %s: carried %q, want %q", tc.caller.Provider, tc.source.Provider, source, tc.model, got.Model, tc.want)
			}
		}
	}
}
