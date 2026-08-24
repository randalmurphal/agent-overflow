package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// command_result_suppression_test.go — the two-part rule that decides whether
// a provider-executed command's output becomes a transcript row: the SEND-time
// candidacy, the REPLY-time confirmation, and the uuid correlation that
// carries the first to the second.
//
// All three have to be pinned together: a rule that suppressed everything
// would pass a "no row" assertion, a correlation that leaked would suppress
// somebody else's command, and a reply test that matched anything would hide
// the errors this split exists to keep.

func TestCommandSuppressionCandidateClassifiesEverySendShape(t *testing.T) {
	command := provider.SendOptions{}
	internal := provider.SendOptions{InternalCommand: true}
	guarded := provider.SendOptions{GuardClaudeSlashCommand: true}

	cases := []struct {
		name          string
		content       string
		opts          provider.SendOptions
		want          bool
		unconditional bool
		why           string
	}{
		// AO's own bookkeeping. The user never typed it, and its failures are
		// surfaced by AO's own reconcilers, so the reply text gets no vote.
		{"internal rename", "/rename AO Thread One", internal, true, true,
			"a command AO issued for itself must not appear in the user's transcript"},
		{"internal effort write", "/effort xhigh", internal, true, true,
			"the live-config path issues this; the reconciler owns its failures"},

		// State echoes: the output CAN be a restatement of something AO
		// renders live, so the send is a candidate and the reply decides.
		{"effort tier", "/effort xhigh", command, true, false, "the composer already shows the effort tier"},
		{"fast on", "/fast on", command, true, false, "the fast-mode chip already shows this"},
		{"fast off", "/fast off", command, true, false, "the fast-mode chip already shows this"},
		{"model switch", "/model claude-opus-4-6", command, true, false, "the thread header already shows the model"},
		{"case and spacing", "/EFFORT   High  ", command, true, false,
			"the CLI's router is not case sensitive and neither is this"},
		{"leading whitespace", " /effort xhigh", command, false, false,
			"the CLI tests the raw string, so a leading space makes this prose and no command runs"},

		// The bare forms are QUESTIONS. Their answer IS the row.
		{"bare effort", "/effort", command, false, false, "prints the current effort level — that is the answer"},
		{"effort current", "/effort current", command, false, false, "same question, spelled out"},
		{"bare model", "/model", command, false, false, "reports the current model rather than switching"},
		{"bare fast", "/fast", command, false, false, "not a recognised toggle argument"},
		{"unknown effort tier", "/effort bogus", command, false, false,
			"the CLI answers 'Invalid argument' — an answer, not a confirmation"},

		// Everything else keeps its row.
		{"usage", "/usage", command, false, false, "the row is what the user asked for"},
		{"context", "/context", command, false, false, "the row is what the user asked for"},
		{"cost", "/cost", command, false, false, "the row is what the user asked for"},
		{"status", "/status", command, false, false, "the row is what the user asked for"},
		{"skill", "/my-skill do the thing", command, false, false, "skills and plugin commands keep their rows"},
		{"plugin command", "/plugin:thing", command, false, false, "skills and plugin commands keep their rows"},

		// A guarded send never reaches the CLI's command router at all, so
		// there is no command output to suppress — including one whose text
		// happens to look like a command.
		{"guarded state echo", "/effort xhigh", guarded, false, false,
			"the slash guard prefixes a newline; this arrives at the model as prose"},
		{"guarded prose", "just some prose", guarded, false, false, "not command-shaped"},
		{"command-shaped path", "/etc/hosts is interesting", command, false, false,
			"an interior slash is not a command name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, got := commandSuppressionCandidate(tc.content, tc.opts)
			if got != tc.want {
				t.Fatalf("commandSuppressionCandidate(%q) candidate = %v, want %v — %s",
					tc.content, got, tc.want, tc.why)
			}
			if got && candidate.unconditional != tc.unconditional {
				t.Fatalf("commandSuppressionCandidate(%q) unconditional = %v, want %v — "+
					"an AO-issued command ignores the reply text and a user-typed one must not",
					tc.content, candidate.unconditional, tc.unconditional)
			}
		})
	}
}

// The whole point of the split. Every string here comes from the installed
// 2.1.237 bundle's `type: "local"` command implementations (offsets cited in
// command_result_suppression.go) or from the checked-in
// effort_live_20260812.ndjson capture. A refusal, a usage line, a partial
// success, and anything unrecognised must all keep their row: the user typed
// the command, and if it failed nothing else in the app will say so.
func TestCommandOutputConfirmsStateKeepsEveryNonConfirmation(t *testing.T) {
	cases := []struct {
		name      string
		candidate commandSuppression
		text      string
		want      bool
		why       string
	}{
		// /model — the defect this split was written for.
		{"model accepted", commandSuppression{name: "model", argument: "claude-opus-4-6"},
			"Set model to claude-opus-4-6 for this session only", true,
			"the header already shows the model"},
		{"model accepted and saved", commandSuppression{name: "model", argument: "claude-opus-4-6"},
			"Set model to claude-opus-4-6 and saved as your default for new sessions", true,
			"still a plain confirmation of the switch that was asked for"},
		{"model rejected by server probe", commandSuppression{name: "model", argument: "claude-opus-9"},
			"Model 'claude-opus-9' not found", false,
			"the resolver forwards a server message verbatim; nothing else tells the user it failed"},
		{"model restricted by org", commandSuppression{name: "model", argument: "claude-opus-4-6"},
			"Model 'claude-opus-4-6' is restricted by your organization's settings. Run /model to choose a different model.", false,
			"a refusal is not a confirmation"},
		{"model substituted by org", commandSuppression{name: "model", argument: "claude-opus-4-6"},
			"Model \"claude-opus-4-6\" is restricted by your organization's settings. Using claude-sonnet-5 instead.\nSet model to claude-sonnet-5 for this session only", false,
			"the user got a model they did not ask for, and this sentence is the only place that is said"},
		{"model usage line", commandSuppression{name: "model", argument: "claude-opus-4-6"},
			"Usage: /model <name>. Available: claude-opus-5, default, or a full model ID.", false,
			"a usage line is an answer"},

		// /effort — the checked-in capture's own strings.
		{"effort accepted", commandSuppression{name: "effort", argument: "low"},
			"Set effort level to low (this session only): Quick, straightforward implementation with minimal overhead", true,
			"the composer already shows the tier"},
		{"effort invalid argument", commandSuppression{name: "effort", argument: "low"},
			"Invalid argument: bogus. Valid options are: low, medium, high, xhigh, max, ultracode, auto", false,
			"a rejection keeps its row even when the send looked like a state echo"},
		{"effort clamped by org", commandSuppression{name: "effort", argument: "xhigh"},
			"Effort 'xhigh' exceeds your organization's limit for claude-opus-5; set to 'high' instead (this session only): deep reasoning", false,
			"the session is running a tier the user did not ask for"},
		{"effort setter failed", commandSuppression{name: "effort", argument: "xhigh"},
			"Failed to set effort level: network unreachable", false, "a failure is user-facing state"},

		// /fast — the ON reply leads with a glyph, so containment, not prefix.
		{"fast on accepted", commandSuppression{name: "fast", argument: "on"},
			"↯ Fast mode ON · claude-opus-5", true, "the fast-mode chip already shows this"},
		{"fast on with implicit model switch", commandSuppression{name: "fast", argument: "on"},
			"↯ Fast mode ON · model set to claude-opus-5 · fast", true,
			"the glyph and the model note both precede the state words"},
		{"fast off accepted", commandSuppression{name: "fast", argument: "off"},
			"Fast mode OFF (this session only)", true, "the fast-mode chip already shows this"},
		{"fast declined by the SDK gate", commandSuppression{name: "fast", argument: "on"},
			"Fast mode unavailable: Fast mode is not available in the Agent SDK", false,
			"the toggle did not land; the transcript is where the user finds out"},
		{"fast declined by the account gate", commandSuppression{name: "fast", argument: "on"},
			"Fast mode unavailable: Fast mode requires usage credits · /usage-credits to turn them on", false,
			"an actionable refusal must not be swallowed"},
		{"fast bad argument", commandSuppression{name: "fast", argument: "on"},
			"Unknown argument \"onn\". Use: /fast [on|off]", false, "a usage line is an answer"},
		{"fast off answered with on", commandSuppression{name: "fast", argument: "off"},
			"↯ Fast mode ON · claude-opus-5", false,
			"the reply must confirm the state that was ASKED for, not any state"},

		// Fail open on everything this file has never seen.
		{"unrecognised copy drift", commandSuppression{name: "effort", argument: "low"},
			"Effort level is now low.", false,
			"CLI copy drift costs an unwanted row, never a hidden error"},
		{"empty output", commandSuppression{name: "effort", argument: "low"}, "   ", false,
			"nothing was said, so nothing was confirmed"},
		{"unknown command name", commandSuppression{name: "usage"},
			"Set effort level to low (this session only)", false,
			"only the state-echo commands have a confirmation vocabulary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandOutputConfirmsState(tc.candidate, tc.text); got != tc.want {
				t.Fatalf("commandOutputConfirmsState(%+v, %q) = %v, want %v — %s",
					tc.candidate, tc.text, got, tc.want, tc.why)
			}
		})
	}
}

// The decision is carried by the command uuid the CLI echoes on its lifecycle
// bracket. A uuid AO did not mark keeps its row whatever the text says.
func TestSuppressedCommandResultIsScopedToItsOwnCommandUUID(t *testing.T) {
	const confirmed = "Set effort level to xhigh (this session only): deep reasoning"

	s := &Session{}
	s.noteSuppressedCommandResult("uuid-effort", "/effort xhigh",
		provider.SendOptions{})
	s.noteSuppressedCommandResult("uuid-usage", "/usage",
		provider.SendOptions{})

	if !s.commandResultRowSuppressed("uuid-effort", confirmed) {
		t.Fatal("the confirmed /effort send was not suppressed")
	}
	if s.commandResultRowSuppressed("uuid-usage", confirmed) {
		t.Fatal("/usage was suppressed — a command whose row IS the answer")
	}
	if s.commandResultRowSuppressed("uuid-never-sent", confirmed) {
		t.Fatal("an unknown uuid classified as suppressed")
	}

	// Non-consuming: one command can carry its output more than once (the
	// `result` envelope repeats the same text), and every carrier has to
	// classify the same way.
	if !s.commandResultRowSuppressed("uuid-effort", confirmed) {
		t.Fatal("a second read of the same uuid answered differently")
	}

	// Released by the terminal lifecycle frame, so the decision cannot
	// outlive the bracket it was made for.
	s.releaseSuppressedCommandResult("uuid-effort")
	if s.commandResultRowSuppressed("uuid-effort", confirmed) {
		t.Fatal("the suppression outlived its command's bracket")
	}
}

// The candidacy is send-time and the verdict is reply-time, on ONE uuid: the
// same marked send suppresses a confirmation and keeps a refusal.
func TestSuppressedCandidateStillKeepsARefusalRow(t *testing.T) {
	s := &Session{}
	s.noteSuppressedCommandResult("uuid-model", "/model claude-opus-9",
		provider.SendOptions{})

	if s.commandResultRowSuppressed("uuid-model", "Model 'claude-opus-9' not found") {
		t.Fatal("a rejected /model slug lost the only row that says the command failed")
	}
	if !s.commandResultRowSuppressed("uuid-model", "Set model to claude-opus-9 for this session only") {
		t.Fatal("the confirmation of the same send was not suppressed")
	}
}

// An AO-issued command is suppressed on text that confirms nothing, because
// AO's reconcilers own its failures and a text test would put AO's bookkeeping
// back in the transcript the first time the CLI's copy moved.
func TestInternalCommandSuppressionIgnoresTheReplyText(t *testing.T) {
	s := &Session{}
	s.noteSuppressedCommandResult("uuid-rename", "/rename AO Thread One",
		provider.SendOptions{InternalCommand: true})

	for _, text := range []string{
		`Session renamed to: AO Thread One`,
		`Session renamed to: AO Thread One 2 ("AO Thread One" is held by another live session on this machine)`,
		`Rename failed: name too long`,
		``,
	} {
		if !s.commandResultRowSuppressed("uuid-rename", text) {
			t.Fatalf("an AO-issued command kept a row for %q", text)
		}
	}
}

// End to end through the parser, on the checked-in live-config capture. The
// first command is a user-typed `/effort low` the CLI confirmed; the second is
// `/effort bogus`, never a candidate; the third is a user-typed `/fast on` the
// CLI DECLINED — the case the old send-time-only rule swallowed. The event
// itself still reaches the app layer in all three, because the live-config
// reconciler settles an apply from exactly this output.
func TestCommandResultEventCarriesTheSuppressionAndStillEmits(t *testing.T) {
	const effortUUID = "aaaaaaa1-0000-4000-8000-000000000001"
	const fastUUID = "aaaaaaa3-0000-4000-8000-000000000003"

	s := &Session{}
	// What Send would have recorded for two commands the user typed. The
	// middle command in the fixture (`/effort bogus`) is left unmarked: its
	// argument is not a live tier, so it was never a candidate.
	s.noteSuppressedCommandResult(effortUUID, "/effort low",
		provider.SendOptions{})
	s.noteSuppressedCommandResult(fastUUID, "/fast on",
		provider.SendOptions{})

	parser := NewParser()
	parser.peerTurns = s

	type got struct {
		uuid       string
		suppressed bool
	}
	var results []got
	for _, line := range fixtureLines(t, effortLiveFixture) {
		events, err := parser.ParseLine("thread-1", line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		for _, evt := range events {
			if evt.Kind != provider.EventCommandResult {
				continue
			}
			var meta provider.CommandResultMeta
			if len(evt.Meta) > 0 {
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal command result meta: %v", err)
				}
			}
			if evt.Content == "" {
				t.Fatalf("suppression dropped the output of %s; it is a ROW decision, "+
					"and the live-config reconciler reads this text", meta.CommandUUID)
			}
			results = append(results, got{uuid: meta.CommandUUID, suppressed: meta.Suppressed})
		}
	}

	want := []got{
		{effortUUID, true},
		{"aaaaaaa2-0000-4000-8000-000000000002", false},
		// "Fast mode unavailable: Fast mode is not available in the Agent SDK".
		// The user typed /fast on, it did not happen, and this row is the only
		// place the app says so.
		{fastUUID, false},
	}
	if len(results) != len(want) {
		t.Fatalf("command results = %+v, want %d", results, len(want))
	}
	for i, w := range want {
		if results[i] != w {
			t.Fatalf("result %d = %+v, want %+v", i, results[i], w)
		}
	}

	// Every bracket in the fixture terminated, so nothing may be left
	// holding a decision for a uuid a later command could reuse.
	for _, uuid := range []string{effortUUID, fastUUID} {
		if s.commandResultRowSuppressed(uuid, "Set effort level to low (this session only): x") {
			t.Fatalf("%s stayed suppressed after its bracket completed", uuid)
		}
	}
}

// A parser with no session (every parser unit test builds one) must not
// suppress anything and must not panic: with no ledger, no uuid can be proven
// suppressed, which is also the correct answer.
func TestCommandResultSuppressionIsInertWithoutASession(t *testing.T) {
	parser := NewParser()
	for _, line := range fixtureLines(t, effortLiveFixture) {
		events, err := parser.ParseLine("thread-1", line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		for _, evt := range events {
			if evt.Kind != provider.EventCommandResult {
				continue
			}
			var meta provider.CommandResultMeta
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal command result meta: %v", err)
			}
			if meta.Suppressed {
				t.Fatalf("a session-less parser suppressed %s", meta.CommandUUID)
			}
		}
	}
}
