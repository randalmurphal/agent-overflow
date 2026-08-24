package claude

import (
	"log"
	"strings"
	"sync"

	"agent-overflow/internal/provider"
)

// command_result_suppression.go — which provider-executed slash commands keep
// a transcript row, and which do not.
//
// The Claude CLI answers every LOCAL command with a `<synthetic>`-model
// assistant envelope, and triage turns that envelope into a `command_result`
// row (internal/triage/command_result.go). For `/usage`, `/context`, `/cost`,
// `/status`, a skill, or a plugin command that row IS the answer the user
// asked for and must stay. Two families are not:
//
//   - Commands Agent Overflow issues on its OWN behalf. `/rename` keeps a
//     session's peer-visible name in step with its thread title
//     (session_peer.go), and `/effort` / `/fast` are how a live-config apply
//     reaches a running process (live_update.go sendConfigCommand). The user
//     never typed any of them, so their output is AO's bookkeeping leaking
//     into a history the user is meant to read as their own conversation.
//   - Commands the USER typed whose output is a CONFIRMATION of state AO
//     already renders itself. `/effort xhigh`, `/fast on` and `/model <slug>`
//     all answer with one line restating a value the composer and the thread
//     header show live.
//
// THE DECISION IS SPLIT ACROSS THE TWO MOMENTS THAT HAVE THE FACTS.
//
// At SEND time a command becomes a suppression CANDIDATE, and AO's own writes
// become unconditional. Candidacy is all send time can honestly decide: no
// reply exists yet, so "the user asked to switch models" is knowable and "the
// switch was accepted" is not.
//
// At REPLY time a candidate is suppressed only if the text is a RECOGNISED
// confirmation for the command that was sent. A refusal, a usage line, an
// org-policy substitution, or anything this file does not recognise keeps its
// row. That direction is deliberate and load-bearing: `/model <slug>` with a
// slug the CLI rejects answers with an error whose wording AO cannot predict
// (the 2.1.237 resolver forwards a SERVER probe's message verbatim), and
// suppressing it would mean the user typed a command, it failed, and nothing
// anywhere said so — errors are user-facing state, not log entries (root
// CLAUDE.md principle 5).
//
// FAIL OPEN EVERYWHERE. Every unrecognised reply keeps its row, so CLI copy
// drift costs one unwanted state-echo row rather than a hidden error. That is
// also why AO's OWN commands are unconditional rather than text-tested: their
// failures are surfaced by the live-config reconciler and the peer-rename
// read-back (app_claude_live_config.go, session_peer.go), so a row would be
// noise, and making them depend on matching CLI copy would put AO's
// bookkeeping back in the transcript the first time a string moved.
//
// Both halves are correlated by the command uuid the CLI echoes on the
// lifecycle bracket — the same correlation the peer-turn ledger already runs
// on (session_peer.go), for the same reason: AO stamped it, so AO can answer
// for it. The reply-time test reads only the text of the envelope in hand and
// holds no state across envelopes.
//
// The suppression is a ROW decision and nothing more. The command still runs,
// its `command_lifecycle` bracket still settles, `/rename` still promotes its
// name, and the output event still reaches the app-layer reconciler that
// settles an /effort or /fast apply from it (app_claude_live_config.go).
//
// A CLI old enough to emit no `command_lifecycle` acks correlates nothing, so
// nothing is suppressed there and every command keeps its row — the same
// posture every other consumer of the bracket takes (claude-wire.md
// §command_lifecycle: absence proves nothing).

// stateEchoCommands are the command names whose ARGUMENT form CAN answer with
// a confirmation of state AO renders in its own UI. Membership makes a send a
// CANDIDATE; whether the reply is actually that confirmation is decided later
// by commandOutputConfirmsState.
//
// The argument is part of the test, because the bare forms are QUESTIONS whose
// answer is the row: `/effort` and `/effort current` print "Current effort
// level: …", and `/model` with no slug reports the current model rather than
// switching. Only a recognised argument — a live effort tier, an on/off fast
// toggle, a model slug — can make the reply a restatement rather than an
// answer.
var stateEchoCommands = map[string]func(argument string) bool{
	"effort": IsLiveEffortTier,
	"fast": func(argument string) bool {
		return argument == string(FastModeOn) || argument == string(FastModeOff)
	},
	// Any non-empty argument is a switch request. There is no closed model
	// vocabulary to check against here (the catalog is app-level and moves
	// with the CLI), and — since v2 of this rule — there no longer needs to
	// be: a slug the CLI rejects fails the reply-time confirmation test and
	// keeps its error row.
	"model": func(argument string) bool { return argument != "" },
}

// The confirmation vocabulary, read out of the installed CLI bundle at
// ~/.local/share/claude/versions/2.1.237 rather than guessed. Offsets are into
// that file; the `type: "local"` command implementations are the ones that
// matter, because those are what `supportsNonInteractive` dispatches to and
// what becomes a `<synthetic>` assistant envelope. The interactive TUI has a
// SECOND set of strings for the same commands ("Model set to …", a `setHint`
// path at @322018331) which never reaches this parser.
const (
	// effortSetReplyPrefix — the /effort setter's success line, built at
	// @311408961 as:
	//
	//	`Set effort level to ${L_e(i)}${p}: ${d}${l??""}`
	//
	// where p is " (this session only)" or " (saved as your default for new
	// sessions)". Deliberately NOT matched: the sibling return one branch
	// above it, `Effort '<x>' exceeds your organization's limit for <model>;
	// set to '<y>' instead…`, which sets a tier the user did not ask for and
	// is exactly the kind of partial success the transcript should show.
	effortSetReplyPrefix = "Set effort level to "

	// modelSetReplyPrefix — the /model setter's success line, built inside
	// WYn at @310115005 as:
	//
	//	`Set model to ${rr.bold(IR(e))}${n?" and saved as your default…":" for this session only"}`
	//
	// The bold wraps the model NAME, so the prefix stays plain whether or not
	// the CLI emits colour. `flw` (@311387316) can prepend an org-substitution
	// notice and a newline before it — `Model "X" is restricted by your
	// organization's settings. Using Y instead.` — which is why this is
	// matched as a PREFIX of the whole reply: a substituted model is not the
	// model the user asked for, and that sentence is the only place it is
	// said.
	modelSetReplyPrefix = "Set model to "

	// fastModeOnReplyText / fastModeOffReplyText — the /fast toggle's success
	// lines, built inside eFi at @311324360 as:
	//
	//	`${Qtt(!0)} Fast mode ON${c} \xB7 ${p}${o?"":" (this session only)"}`
	//	`Fast mode OFF${o?"":" (this session only)"}`
	//
	// Qtt(true) returns the colourised FUe glyph ("↯", the glyph table at
	// @298504321), and c is " \xB7 model set to <model>" when enabling also
	// switched the model. The ON line therefore has content BEFORE the state
	// words, which is why these two are containment tests rather than
	// prefixes.
	//
	// No refusal collides with either. The gate messages (@299752420 and
	// @299753447) read "Fast mode unavailable: …", "Fast mode requires a paid
	// subscription", "Fast mode has been disabled by your organization", "Fast
	// mode is currently unavailable"; the bad-argument reply (@311325100) is
	// `Unknown argument "<x>". Use: /fast [on|off]`. None contains the
	// upper-case state word, so all of them keep their rows.
	fastModeOnReplyText  = "Fast mode ON"
	fastModeOffReplyText = "Fast mode OFF"
)

// commandSuppression is the send-time record for one outbound command uuid:
// what AO sent, and whether the reply gets a vote.
type commandSuppression struct {
	// unconditional marks a command AO issued on its own behalf. Its reply
	// text decides nothing — see the fail-open note in the file header.
	unconditional bool
	// name and argument are the outbound command as the CLI's own router
	// would split it, kept so the reply-time test knows which confirmation it
	// is looking for. Empty when unconditional.
	name     string
	argument string
}

// commandSuppressionCandidate answers, for one outbound send, whether the row
// its output would produce is a suppression candidate, and on what terms.
//
// Every input is a send-time fact: the options the caller set and the text AO
// is about to write. Nothing here reads a reply, and nothing here is final for
// a user-typed command — commandOutputConfirmsState finishes the decision when
// the reply arrives.
func commandSuppressionCandidate(content string, opts provider.SendOptions) (commandSuppression, bool) {
	if opts.GuardClaudeSlashCommand || !startsWithCommandShapedWord(content) {
		// Guarded sends never reach the CLI's command router at all
		// (slash_guard.go), and non-command text produces no command output to
		// suppress.
		return commandSuppression{}, false
	}
	if opts.InternalCommand {
		return commandSuppression{unconditional: true}, true
	}
	name, argument := outboundSlashCommand(content)
	recognises, ok := stateEchoCommands[name]
	if !ok || !recognises(argument) {
		return commandSuppression{}, false
	}
	return commandSuppression{name: name, argument: argument}, true
}

// commandOutputConfirmsState reports whether text is a RECOGNISED confirmation
// that the candidate command did what it was asked to do.
//
// False for every refusal, every usage line, every partial success, and every
// string this file has never seen — the fail-open direction. The caller only
// reaches here for a command the user typed, so the cost of a false negative
// is one redundant state-echo row and the cost of a false positive is a
// silently swallowed error.
func commandOutputConfirmsState(candidate commandSuppression, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	switch candidate.name {
	case "effort":
		return strings.HasPrefix(text, effortSetReplyPrefix)
	case "model":
		return strings.HasPrefix(text, modelSetReplyPrefix)
	case "fast":
		switch candidate.argument {
		case string(FastModeOn):
			return strings.Contains(text, fastModeOnReplyText)
		case string(FastModeOff):
			return strings.Contains(text, fastModeOffReplyText)
		}
	}
	return false
}

// outboundSlashCommand splits a command-shaped send into its lowercased name
// and the rest of the first line, trimmed.
//
// It uses the same byte classes as the outbound slash guard
// (startsWithCommandShapedWord), because the question is the same one: what
// will the CLI's own router treat as the command name. A send that is not
// command-shaped answers ("", "").
func outboundSlashCommand(content string) (name, argument string) {
	if !startsWithCommandShapedWord(content) {
		return "", ""
	}
	end := len(content)
	for i := 1; i < len(content); i++ {
		if !isCommandNameByte(content[i]) {
			end = i
			break
		}
	}
	name = strings.ToLower(content[1:end])
	argument, _, _ = strings.Cut(strings.TrimSpace(content[end:]), "\n")
	return name, strings.ToLower(strings.TrimSpace(argument))
}

// maxTrackedSuppressedCommandUUIDs bounds the suppression ledger.
//
// Entries are released by the terminal lifecycle frame, exactly like the
// issued-uuid ledger next to it, so a healthy session holds at most one per
// in-flight config write. At the cap the ledger REFUSES new entries rather
// than evicting: a refused entry costs one unwanted row in the transcript,
// while evicting a live one would suppress a row that a still-running command
// has not produced yet. Both are wrong; only the second can hide a user's
// answer.
const maxTrackedSuppressedCommandUUIDs = 64

// suppressedCommandUUIDs is the ledger of outbound command uuids whose output
// row is a suppression candidate, keyed to what was sent.
//
// Concurrency mirrors issuedCommandUUIDs: Send writes it from caller
// goroutines, the assistant and lifecycle parses read and release it on the
// read loop.
type suppressedCommandUUIDs struct {
	mu       sync.Mutex
	uuids    map[string]commandSuppression
	overflow bool
}

func (l *suppressedCommandUUIDs) note(uuid string, candidate commandSuppression) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uuids == nil {
		l.uuids = make(map[string]commandSuppression, 2)
	}
	if _, seen := l.uuids[uuid]; seen {
		return
	}
	if len(l.uuids) >= maxTrackedSuppressedCommandUUIDs {
		if !l.overflow {
			l.overflow = true
			log.Printf(
				"claude: command-result suppression ledger hit its %d-entry cap; further internal commands will keep their transcript rows for the rest of this session",
				maxTrackedSuppressedCommandUUIDs)
		}
		return
	}
	l.uuids[uuid] = candidate
}

// candidate returns the send-time record for a uuid. It does NOT consume: one
// command can emit its output envelope more than once (the `result` envelope
// repeats the same text), and every carrier must classify the same way.
// Release happens once, on the terminal lifecycle frame.
func (l *suppressedCommandUUIDs) candidate(uuid string) (commandSuppression, bool) {
	if uuid == "" {
		return commandSuppression{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	candidate, ok := l.uuids[uuid]
	return candidate, ok
}

func (l *suppressedCommandUUIDs) release(uuid string) {
	if uuid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.uuids, uuid)
}

// noteSuppressedCommandResult records, before the write reaches stdin, that
// this send's command output is a suppression candidate. Called from Send so
// every issuer — the live-config writer, the peer rename, and a user-typed
// command off the composer palette — is covered by one rule.
func (s *Session) noteSuppressedCommandResult(uuid, content string, opts provider.SendOptions) {
	candidate, ok := commandSuppressionCandidate(content, opts)
	if !ok {
		return
	}
	s.suppressedCommands.note(uuid, candidate)
}

// commandResultRowSuppressed reports whether the output arriving inside this
// command's lifecycle bracket keeps its transcript row. text is the output
// itself — the second half of the decision, and the reason a rejected `/model`
// slug still reaches the user.
func (s *Session) commandResultRowSuppressed(commandUUID, text string) bool {
	candidate, ok := s.suppressedCommands.candidate(commandUUID)
	if !ok {
		return false
	}
	if candidate.unconditional {
		return true
	}
	return commandOutputConfirmsState(candidate, text)
}

// releaseSuppressedCommandResult drops a uuid whose bracket has terminated.
func (s *Session) releaseSuppressedCommandResult(commandUUID string) {
	s.suppressedCommands.release(commandUUID)
}
