package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

const localCommandFixture = "../../../docs/references/fixtures/claude/local_command_20260803.ndjson"

func fixtureLines(t *testing.T, path string) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, []byte(line))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture %s: %v", path, err)
	}
	return lines
}

// TestLocalCommandFixtureProducesOneCommandResultRow walks the full envelope
// sequence a zero-token local command produces (verified 2.1.219, 2026-08-03
// live probe) and pins the three routing decisions that make it render
// correctly:
//
//   - the `<synthetic>` assistant envelope becomes exactly ONE
//     EventCommandResult, never assistant text;
//   - the `<command-name>` metadata echo on the isReplay user envelope is
//     FORWARDED as exactly one `command_echo`-flagged EventUserText carrying
//     the send's uuid — it must reach triage to consume the pending-send
//     entry (a suppressed echo strands the entry and poisons turn indexing —
//     incident 2026-08-04); triage's unmatched branch keeps the XML off the
//     timeline;
//   - `result.result` repeats the same text and produces no second row.
func TestLocalCommandFixtureProducesOneCommandResultRow(t *testing.T) {
	parser := &Parser{}
	counts := map[provider.EventKind]int{}
	var commandResults []provider.ProviderEvent
	var userTexts []provider.ProviderEvent

	for _, line := range fixtureLines(t, localCommandFixture) {
		events, err := parser.ParseLine("thread-1", line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		for _, evt := range events {
			counts[evt.Kind]++
			if evt.Kind == provider.EventCommandResult {
				commandResults = append(commandResults, evt)
			}
			if evt.Kind == provider.EventUserText {
				userTexts = append(userTexts, evt)
			}
		}
	}

	if got := counts[provider.EventCommandResult]; got != 1 {
		t.Fatalf("command result events = %d, want 1 (counts: %v)", got, counts)
	}
	if got := counts[provider.EventUserText]; got != 1 {
		t.Fatalf("user text events = %d, want 1 — the <command-name> echo must reach triage to consume its pending send", got)
	}
	var echoMeta map[string]any
	if err := json.Unmarshal(userTexts[0].Meta, &echoMeta); err != nil {
		t.Fatalf("unmarshal command echo meta: %v", err)
	}
	if flagged, _ := echoMeta["command_echo"].(bool); !flagged {
		t.Fatalf("command echo meta = %s, want command_echo=true so triage's unmatched branch can drop the XML", userTexts[0].Meta)
	}
	if id, _ := echoMeta["provider_item_id"].(string); id != "f47ac10b-58cc-4372-a567-0e02b2c3d479" {
		t.Fatalf("command echo provider_item_id = %q, want the send's client-minted uuid", id)
	}
	if got := counts[provider.EventContentBlockStop]; got != 0 {
		t.Fatalf("content block stop events = %d, want 0 — the synthetic envelope must not recover as assistant text", got)
	}
	if got := counts[provider.EventTurnComplete]; got != 1 {
		t.Fatalf("turn complete events = %d, want 1", got)
	}
	if got := counts[provider.EventCommandsChanged]; got != 1 {
		t.Fatalf("commands changed events = %d, want 1", got)
	}

	result := commandResults[0]
	if !strings.HasPrefix(result.Content, "Current session") {
		t.Fatalf("command result content = %q", result.Content)
	}
	if !result.ContentPresent {
		t.Fatal("command result must carry ContentPresent so an empty body is distinguishable")
	}
	if result.ItemID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("command result item id = %q, want the synthetic message id", result.ItemID)
	}
}

func TestSyntheticAssistantWithErrorEnumStaysOnTheErrorPath(t *testing.T) {
	// `<synthetic>` is also the model on the CLI's own API-error message
	// (createAssistantAPIErrorMessage). The error enum takes precedence, so
	// that envelope must still produce EventError and no command result.
	line := []byte(`{"type":"assistant","message":{"id":"m1","model":"<synthetic>","role":"assistant",` +
		`"error":"rate_limit","content":[{"type":"text","text":"API Error: rate limit reached"}]}}`)
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	var kinds []provider.EventKind
	for _, evt := range events {
		kinds = append(kinds, evt.Kind)
	}
	for _, kind := range kinds {
		if kind == provider.EventCommandResult {
			t.Fatalf("error envelope produced a command result (kinds: %v)", kinds)
		}
	}
	found := false
	for _, evt := range events {
		if evt.Kind == provider.EventError {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EventError, got kinds %v", kinds)
	}
}

func TestSyntheticAssistantWithEmptyOutputEmitsNothing(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"m1","model":"<synthetic>","role":"assistant",` +
		`"content":[{"type":"text","text":"   \n"}]}}`)
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none for a command that printed nothing", events)
	}
}

// TestSyntheticAssistantDoesNotClaimTheTurnsAssistantMessageID pins that a
// command result is not model output: the trailing `result` envelope's
// assistant_message_id must stay whatever the real model last produced.
func TestSyntheticAssistantDoesNotClaimTheTurnsAssistantMessageID(t *testing.T) {
	parser := &Parser{}
	real := []byte(`{"type":"assistant","message":{"id":"msg_real","model":"claude-opus-5","role":"assistant",` +
		`"content":[{"type":"text","text":"hi"}]}}`)
	if _, err := parser.ParseLine("thread-1", real); err != nil {
		t.Fatalf("ParseLine(real): %v", err)
	}
	synthetic := []byte(`{"type":"assistant","message":{"id":"msg_synthetic","model":"<synthetic>","role":"assistant",` +
		`"content":[{"type":"text","text":"usage report"}]}}`)
	if _, err := parser.ParseLine("thread-1", synthetic); err != nil {
		t.Fatalf("ParseLine(synthetic): %v", err)
	}
	events, err := parser.ParseLine("thread-1", []byte(`{"type":"result","subtype":"success"}`))
	if err != nil {
		t.Fatalf("ParseLine(result): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("result events = %d, want 1", len(events))
	}
	meta, ok := events[0].TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok {
		t.Fatalf("turn complete meta = %T", events[0].TurnComplete)
	}
	if meta.AssistantMessageID != "msg_real" {
		t.Fatalf("assistant_message_id = %q, want msg_real", meta.AssistantMessageID)
	}
}

func TestExtractSessionInfoCarriesCommandDiscovery(t *testing.T) {
	line := fixtureLines(t, localCommandFixture)[2]
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine(init): %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one init", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	wantCommands := []string{"usage", "context", "compact", "review", "mcp__playwright__browser_snapshot", "ship-it"}
	if strings.Join(info.SlashCommands, ",") != strings.Join(wantCommands, ",") {
		t.Fatalf("slash commands = %v, want %v", info.SlashCommands, wantCommands)
	}
	if strings.Join(info.Skills, ",") != "ship-it" {
		t.Fatalf("skills = %v, want [ship-it]", info.Skills)
	}
	if len(info.Plugins) != 1 || info.Plugins[0].Name != "release-tools" ||
		info.Plugins[0].Source != "local" || info.Plugins[0].Path == "" {
		t.Fatalf("plugins = %+v", info.Plugins)
	}
}

// TestExtractSessionInfoTreatsAbsenceAsSilence — an init envelope from a CLI
// too old to report commands must leave every discovery field nil, so a
// consumer can tell "nobody said" from "none".
func TestExtractSessionInfoTreatsAbsenceAsSilence(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s","model":"m","cwd":"/tmp"}`)
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if info.SlashCommands != nil || info.Skills != nil || info.Plugins != nil {
		t.Fatalf("expected nil discovery fields, got %+v", info)
	}
}

func TestParseCommandsChanged(t *testing.T) {
	line := fixtureLines(t, localCommandFixture)[7]
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine(commands_changed): %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventCommandsChanged {
		t.Fatalf("events = %+v, want one commands_changed", events)
	}
	var meta provider.CommandsChangedMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if len(meta.Commands) != 3 {
		t.Fatalf("commands = %+v, want 3", meta.Commands)
	}
	if meta.Commands[2].Name != "ship-it" ||
		meta.Commands[2].Description != "Release checklist (user)" ||
		meta.Commands[2].ArgumentHint != "[version]" {
		t.Fatalf("third command = %+v", meta.Commands[2])
	}
}

// TestParseCommandsChangedEmptyListIsARealReplacement — `"commands": []` is the
// CLI saying nothing is available, which a live menu must apply. An envelope
// with NO `commands` key says nothing at all and is dropped.
func TestParseCommandsChangedEmptyListIsARealReplacement(t *testing.T) {
	events, err := (&Parser{}).ParseLine("t", []byte(`{"type":"system","subtype":"commands_changed","commands":[]}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one replacement event", events)
	}
	var meta provider.CommandsChangedMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Commands == nil {
		t.Fatal("empty replacement must marshal as [], not null")
	}
	if len(meta.Commands) != 0 {
		t.Fatalf("commands = %+v, want empty", meta.Commands)
	}

	events, err = (&Parser{}).ParseLine("t", []byte(`{"type":"system","subtype":"commands_changed"}`))
	if err != nil {
		t.Fatalf("ParseLine(keyless): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none for an envelope carrying no commands key", events)
	}
}

func TestDecodeWireCommands(t *testing.T) {
	payload := json.RawMessage(`{"commands":[
		{"name":"  usage  ","description":" Show plan usage limits ","argumentHint":""},
		{"name":"","description":"nameless entries are dropped"},
		{"name":"ship-it","description":"Release checklist (user)","argumentHint":"[version]"}
	],"models":[]}`)
	commands, err := decodeWireCommands(payload)
	if err != nil {
		t.Fatalf("decodeWireCommands: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %+v, want 2", commands)
	}
	if commands[0] != (provider.SlashCommand{Name: "usage", Description: "Show plan usage limits"}) {
		t.Fatalf("first command = %+v", commands[0])
	}
}

// TestDecodeWireCommandsAbsenceIsNotAnError — a CLI too old to report commands
// answers nil with no error, which callers must apply as a real (empty) answer.
func TestDecodeWireCommandsAbsenceIsNotAnError(t *testing.T) {
	commands, err := decodeWireCommands(json.RawMessage(`{"models":[]}`))
	if err != nil {
		t.Fatalf("decodeWireCommands: %v", err)
	}
	if commands != nil {
		t.Fatalf("commands = %+v, want nil", commands)
	}
	if commands, err = decodeWireCommands(nil); err != nil || commands != nil {
		t.Fatalf("decodeWireCommands(nil) = %+v, %v", commands, err)
	}
}

func TestNormalizeWireCommandsBoundsTheList(t *testing.T) {
	in := make([]provider.SlashCommand, maxWireCommands+10)
	for i := range in {
		in[i] = provider.SlashCommand{Name: "cmd", Description: strings.Repeat("x", maxCommandDescriptionRunes+50)}
	}
	out := normalizeWireCommands(in)
	if len(out) != maxWireCommands {
		t.Fatalf("len = %d, want %d", len(out), maxWireCommands)
	}
	if got := len([]rune(out[0].Description)); got != maxCommandDescriptionRunes {
		t.Fatalf("description runes = %d, want %d", got, maxCommandDescriptionRunes)
	}
}
