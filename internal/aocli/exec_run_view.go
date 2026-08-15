package aocli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/untrustedtext"
)

// How a run and its phase attempts read on a human line — the one place that
// decides what a caller sees without `--json`. The verbs themselves, and which
// of these lines each one prints, live in exec_run.go.

// runView mirrors only the fields the human lines print. The authoritative shape
// is the app's; `--json` forwards that one verbatim.
type runView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	WorkflowScope       string `json:"workflowScope"`
	Goal                string `json:"goal"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	CurrentPhaseID      string `json:"currentPhaseId"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal"`
	PhaseCount          int    `json:"phaseCount"`
	ParentItemID        string `json:"parentItemId"`
	Resting             bool   `json:"resting"`
	Skipped             bool   `json:"skipped"`
	BoundThreadID       string `json:"boundThreadId"`
	BindingWarning      string `json:"bindingWarning"`
	// FailedUnits is present only on `run status` for a run parked on a failed
	// fan-out; the line prints the ids because they are the second argument of
	// `run retry-unit`.
	FailedUnits []runFailedUnit `json:"failedUnits"`
	// Phases is the run's per-attempt provenance, present on `run status` only.
	Phases []runPhaseAttempt `json:"phases"`
	// Seeds is what the run was started with, present on the single-run reads.
	// It prints under the run line rather than on it: the control verbs share
	// line() and answer "where is it now", which a run's frozen inputs do not.
	Seeds map[string]json.RawMessage `json:"seeds"`
	// Budget is the ceiling in force and the tree's spend against it, present on
	// the single-run reads and only for a run that HAS one. A run with no budget
	// prints no budget line at all — an absent field reads as "no ceiling", and
	// a line saying so on every run would be noise on the surface a reader scans
	// for what the run needs.
	Budget *runBudget `json:"budget"`
	// PendingGuidance is how many `run guide` entries are waiting for this run's
	// next fresh phase entry, present on the single-run reads. Unlike the seeds
	// it DOES print on the run line, and deliberately: the control verbs share
	// that line, and "the resume you just ran continued an attempt and left two
	// steers undelivered" is exactly what their caller has to see.
	PendingGuidance int `json:"pendingGuidance"`
}

// runBudget mirrors the app's budget projection. The app resolves it through
// the SAME call the engine enforces with and computes the percent, so this side
// only decides how it reads — a CLI that recomputed the share would be a second
// answer to "how much is left".
type runBudget struct {
	Kind          string  `json:"kind"`
	CeilingTokens int64   `json:"ceilingTokens"`
	CeilingUSD    float64 `json:"ceilingUsd"`
	CeilingMillis int64   `json:"ceilingMillis"`
	SpentTokens   int64   `json:"spentTokens"`
	SpentUSD      float64 `json:"spentUsd"`
	ElapsedMillis int64   `json:"elapsedMillis"`
	Percent       int     `json:"percent"`
	Estimated     bool    `json:"estimated"`
	UnpricedRows  int64   `json:"unpricedRows"`
	Exhausted     bool    `json:"exhausted"`
	RootItemID    string  `json:"rootItemId"`
}

// line renders one budget as `budget=<spent>/<ceiling> (<n>%)` plus the two
// facts that change what the number means: that part of it was priced from a
// rate table rather than reported by a provider, and that the ceiling is
// already crossed. The units come from the kind, so a reader never has to
// guess whether 25 is dollars or tokens.
func (b runBudget) line() string {
	spent, ceiling := b.spentAndCeiling()
	return fields(
		fmt.Sprintf("budget=%s/%s", spent, ceiling),
		fmt.Sprintf("(%d%%)", b.Percent),
		optionalField("of-run", b.RootItemID),
		estimatedField(b.Estimated),
		unpricedField(b.UnpricedRows),
		exhaustedField(b.Exhausted),
	)
}

func (b runBudget) spentAndCeiling() (string, string) {
	switch b.Kind {
	case "tokens":
		return fmt.Sprintf("%d", b.SpentTokens), fmt.Sprintf("%d tokens", b.CeilingTokens)
	case "usd":
		return fmt.Sprintf("$%.2f", b.SpentUSD), fmt.Sprintf("$%.2f", b.CeilingUSD)
	case "wall_clock":
		return durationText(b.ElapsedMillis), durationText(b.CeilingMillis)
	default:
		// A kind this build does not know is still a real ceiling somewhere: say
		// the percent (already on the line) under a name rather than dropping the
		// whole line and reading as a run with no budget.
		return "?", fmt.Sprintf("? (%s)", b.Kind)
	}
}

func durationText(millis int64) string {
	return (time.Duration(millis) * time.Millisecond).Round(time.Second).String()
}

// estimatedField marks a dollar figure that is partly rate-table priced. It
// prints only when true: the caveat is the exception, and a `estimated=false`
// on every Claude-only run would train a reader to skip the field.
func estimatedField(estimated bool) string {
	if !estimated {
		return ""
	}
	return "estimated=true"
}

// unpricedField says the spend is a LOWER BOUND, not an estimate: this many
// ledger rows carry a model no rate table entry matches. It is printed because
// the consequence is a park — a dollar ceiling the run has not already crossed
// cannot be judged at all, so the run stops at its next phase boundary and this
// line is where the reason is visible before that happens.
func unpricedField(rows int64) string {
	if rows <= 0 {
		return ""
	}
	return fmt.Sprintf("unpriced-rows=%d", rows)
}

func exhaustedField(exhausted bool) string {
	if !exhausted {
		return ""
	}
	return "exhausted=true"
}

// runFailedUnit is what a failed unit reads as. The id is the argument `run
// retry-unit` takes; the note is why the unit is resting failed at all, which
// the status cannot say on its own — a pause tears its in-flight units down
// `failed` carrying an interrupted note, and a reader told only the status
// reads their own pause as a wave of agent failures. The app's document carries
// the try count too; `--json` forwards that one verbatim, as it does for every
// other field this CLI does not model.
type runFailedUnit struct {
	UnitID string `json:"unitId"`
	Note   string `json:"note"`
}

// runPhaseAttempt is one attempt of one phase: what ran it and how its gate
// decided. It answers the two questions a run line cannot — which attempt
// produced the outputs a gate is parked on, and what model settings each
// element actually ran with — for a reader who has only this CLI.
type runPhaseAttempt struct {
	PhaseID  string `json:"phaseId"`
	Attempt  int    `json:"attempt"`
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	// Cause is why the ENGINE parked this attempt, when it was the engine that
	// diagnosed the park — a worktree that would not cut, a phase missing from
	// the frozen snapshot, a spent budget. Empty means the attempt rested on
	// its own envelope, or the reason already names its cause.
	Cause string `json:"cause"`
	// Session is "continued" when the attempt ran on a session an earlier attempt
	// of the same phase started — a loop route's `session: continue`, an answered
	// question, or a finalized takeover. Empty means it started cold.
	Session        string   `json:"session"`
	Decision       string   `json:"decision"`
	DecisionTarget string   `json:"decisionTarget"`
	ExhaustedLoops []string `json:"exhaustedLoops"`
	// Outputs is the bounded digest `run inspect` carries for a phase's latest
	// attempt, and OutputOverflow how many the app left out. `run status`
	// populates neither.
	Outputs        []runOutputDigest `json:"outputs"`
	OutputOverflow int               `json:"outputOverflow"`
}

// runOutputDigest is one envelope output the app already bounded. The CLI only
// decides how it reads: the value is quoted as untrusted data, because it came
// out of a model and the reader is usually another one.
type runOutputDigest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (p runPhaseAttempt) line() string {
	return fields(
		"phase="+p.PhaseID,
		fmt.Sprintf("attempt=%d", p.Attempt),
		"status="+p.Status,
		optionalField("provider", p.Provider),
		optionalField("model", p.Model),
		optionalField("effort", p.Effort),
		optionalField("cause", causeField(p.Cause)),
		optionalField("session", p.Session),
		optionalField("decision", decisionField(p.Decision, p.DecisionTarget)),
		optionalField("exhausted-loops", strings.Join(p.ExhaustedLoops, ",")),
	)
}

// maxCauseRunes bounds a park cause on an attempt line. The engine caps the
// stored text far higher (a wrapped chain of errors), because the durable
// record should not lose the tail; a status block carrying one line per attempt
// is a different budget, and a reader who wants the rest asks for the attempt.
const maxCauseRunes = 300

// causeField quotes a park cause as untrusted data. The engine authored the
// sentence, but the values inside it did not: an argument reference, a branch
// name, a runner's error text all come from a definition or a model.
func causeField(cause string) string {
	if cause == "" {
		return ""
	}
	return untrustedtext.Quote(cause, maxCauseRunes)
}

// decisionField renders a gate outcome. Kind and target are one fact — where the
// gate sent the run — so they print as one field rather than two a reader has to
// pair up, and every surface that shows a decision shares this spelling.
func decisionField(kind, target string) string {
	if kind == "" || target == "" {
		return kind
	}
	return kind + "->" + target
}

func (v runView) line() string {
	phase := v.CurrentPhaseID
	if phase != "" && v.PhaseCount > 0 {
		phase = fmt.Sprintf("%s(%d/%d)", phase, v.CurrentPhaseOrdinal, v.PhaseCount)
	}
	units := make([]string, 0, len(v.FailedUnits))
	for _, unit := range v.FailedUnits {
		units = append(units, unit.UnitID)
	}
	return fields(
		"run="+v.ItemID,
		// Parent sits next to the run id because it is the same fact: which run
		// this is. A campaign's `run list` is otherwise a flat list of ids with
		// the tree that relates them invisible.
		optionalField("parent", v.ParentItemID),
		optionalField("workflow", v.WorkflowID),
		"state="+v.State,
		optionalField("reason", v.Reason),
		optionalField("phase", phase),
		optionalField("failed-units", strings.Join(units, ",")),
		guidanceField(v.PendingGuidance),
		skippedField(v.Skipped),
	)
}

// statusBlock is what `run status` prints: the run line, the seeds the run
// froze at start, then one line per phase attempt. Only the single-run reads
// carry any of it — the app populates seeds and attempts there alone, and the
// control verbs ask "where is it now", which the run line already answers.
func (v runView) statusBlock() string {
	var block strings.Builder
	block.WriteString(v.line())
	v.writeBudgetLine(&block)
	v.writeFailedUnitLines(&block)
	writeSeedLines(&block, v.Seeds)
	v.writeAttemptLines(&block)
	return block.String()
}

// writeBudgetLine prints the ceiling and the tree's spend against it, and
// prints nothing at all for a run that has no ceiling. It sits directly under
// the run line on both single-run blocks — the spend is a fact about where the
// run is, like its state, not about what it was seeded with.
func (v runView) writeBudgetLine(block *strings.Builder) {
	if v.Budget == nil {
		return
	}
	block.WriteString("\n")
	block.WriteString(v.Budget.line())
}

// maxUnitNoteRunes bounds a unit's note on a status block, for the reason
// maxCauseRunes bounds a park cause: the note is a runner's account of how the
// unit ended and can be a wrapped chain of errors, while this block carries one
// line per failed unit. `run inspect --phase <id>` prints it whole.
// The app's wake composer bounds the same value with its own
// `maxFailedUnitNoteRunes`; the two are deliberately independent display
// budgets, not one contract.
const maxUnitNoteRunes = 300

// writeFailedUnitLines prints the account behind each failed unit, under the
// `failed-units=` list on the run line that already names the ids. The status is
// what the ids arrive with and it is not the whole answer: a pause tears its
// in-flight units down `failed` with an interrupted note — there is no
// interrupted unit status, and `failed` is exactly what `run retry-unit`
// recovers — so a reader given only the status reads their own pause as a wave
// of agent failures.
//
// A unit with no note contributes no line: the run line already named it, and a
// line restating the id alone would be the noise this one exists to avoid.
func (v runView) writeFailedUnitLines(block *strings.Builder) {
	for _, unit := range v.FailedUnits {
		note := unitNoteText(unit.Note, maxUnitNoteRunes)
		if note == "" {
			continue
		}
		fmt.Fprintf(block, "\n%s", fields("failed-unit="+unit.UnitID, "note="+note))
	}
}

// unitNoteText is the ONE rendering of a unit's note, shared by the bounded
// `run status` line and `run inspect`'s whole-note field.
//
// It trims once and quotes the TRIMMED value. Both halves matter and both used
// to differ between the two surfaces: `run status` tested a trimmed note for
// emptiness and then quoted the raw one, so a note that was mostly whitespace
// rendered as a wall of `\n` escapes there and as its content in `run inspect`
// — two readings of one record, from the surface a reader reaches for first.
//
// The note is a runner's error text or a repair instruction a human or another
// agent typed, so it is quoted as untrusted data at both. `maxRunes` is the only
// thing the two are allowed to disagree about: naming an attempt is how a caller
// says the bounded form was not enough.
func unitNoteText(note string, maxRunes int) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return ""
	}
	return untrustedtext.Quote(note, maxRunes)
}

// writeAttemptLines is the per-attempt half of both single-run blocks, shared so
// `run status` and `run inspect` cannot end up rendering an attempt two ways.
func (v runView) writeAttemptLines(block *strings.Builder) {
	for _, phase := range v.Phases {
		block.WriteString("\n")
		block.WriteString(phase.line())
		writeDigestLines(block, phase)
	}
}

// writeSeedLines prints one line per seed, sorted, the way `run output` prints
// one per declared output: a run's inputs and its outputs are the same kind of
// answer and read better in the same shape than crammed onto the run line.
func writeSeedLines(block *strings.Builder, seeds map[string]json.RawMessage) {
	names := make([]string, 0, len(seeds))
	for name := range seeds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(block, "\nseed %s=%s", name, string(seeds[name]))
	}
}

// writeDigestLines prints an attempt's output digest under its line, indented so
// the association is unambiguous in a block that already carries several
// attempts. The elision tail is the app's count, restated rather than recomputed:
// the CLI never saw what was left out.
func writeDigestLines(block *strings.Builder, phase runPhaseAttempt) {
	for _, output := range phase.Outputs {
		fmt.Fprintf(block, "\n  output %s=%s", output.Name, untrustedtext.Quote(output.Value, 0))
	}
	if phase.OutputOverflow > 0 {
		fmt.Fprintf(block, "\n  …and %d more (agent-overflow run inspect --phase %s)",
			phase.OutputOverflow, phase.PhaseID)
	}
}

// guidanceField says how many steers are waiting for the run's next phase entry.
// Zero prints nothing: an absent field reads as "nothing pending" and a run that
// has never been guided is every run.
func guidanceField(pending int) string {
	if pending <= 0 {
		return ""
	}
	return fmt.Sprintf("guidance-pending=%d", pending)
}

func skippedField(skipped bool) string {
	if !skipped {
		return ""
	}
	return "skipped=true"
}
