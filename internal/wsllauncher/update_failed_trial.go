package wsllauncher

import (
	"strings"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// The failure memory (docs/specs/app-update.md): a migration or update trial
// that settles without committing is remembered by the build it ran and the
// schema version its database started from. The migration gate does not run
// the same build's trial over the same schema version again on its own; it
// shows the remembered failure with a Retry that does (MigrationRequest.
// Retry). A trial that commits removes the memory, and one that names
// another build or schema version is removed by the gate, which then runs.

// FailedTrialPath is the failure memory beside the record at recordPath.
func FailedTrialPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".failed-trial.json"
}

// trialTrace is what a failed trial is remembered by: the schema version the
// database started from and the last progress the failing step reported.
type trialTrace struct {
	schema int
	phase  string
}

// observe keeps the report's detail unless it is the command's recovery
// after the failure.
func (t *trialTrace) observe(p startupprogress.Progress) {
	if p.Detail != "" && !supervise.RecoveryPhase(p.Phase) {
		t.phase = p.Detail
	}
}

// rememberFailedTrial records that build's trial settled without committing,
// replacing any earlier memory. Without a schema version the failure cannot
// be matched to a later launch, so it is not remembered.
func (s UpdateSequence) rememberFailedTrial(build string, trace trialTrace, reason string) {
	if trace.schema <= 0 {
		s.logf("updater: the failed trial of %s is not remembered: the database's schema version is unknown", build)
		return
	}
	failed := supervise.FailedTrial{
		Build: build, Schema: trace.schema, Reason: reason, Phase: trace.phase, AtMs: s.now().UnixMilli(),
	}
	if err := supervise.SaveFailedTrial(FailedTrialPath(s.RecordPath), failed); err != nil {
		s.logf("updater: remember the failed trial of %s: %v", build, err)
	}
}

// forgetFailedTrial removes the memory. A memory left behind names a build
// or schema version the next migration gate no longer matches, and the gate
// removes it then.
func (s UpdateSequence) forgetFailedTrial() {
	if err := supervise.ForgetFailedTrial(FailedTrialPath(s.RecordPath)); err != nil {
		s.logf("updater: %v", err)
	}
}

// rememberedFailure is the failure memory when it names build's trial over
// schema. A memory of another build or schema version no longer applies and
// is removed. One that cannot be read is logged and treated as none: the
// migration runs, and its outcome replaces or removes the file.
func (s UpdateSequence) rememberedFailure(build string, schema int) (supervise.FailedTrial, bool) {
	failed, found, err := supervise.LoadFailedTrial(FailedTrialPath(s.RecordPath))
	switch {
	case err != nil:
		s.logf("updater: %v; running the database upgrade", err)
		return supervise.FailedTrial{}, false
	case !found:
		return supervise.FailedTrial{}, false
	case failed.Matches(build, schema):
		return failed, true
	}
	s.logf("updater: forgetting the failed trial of %s over schema v%d: this launch runs %s over schema v%d",
		failed.Build, failed.Schema, build, schema)
	s.forgetFailedTrial()
	return supervise.FailedTrial{}, false
}

// rememberedMigrationEnd is the failure page for a migration the memory
// stopped. The page offers Retry.
func rememberedMigrationEnd(failed supervise.FailedTrial) MigrationEnd {
	where := "The last attempt failed."
	if phase := boundedClause(failed.Phase); phase != "" {
		where = "The last attempt stopped at: " + phase + "."
	}
	return MigrationEnd{
		Title:  migrationFailedTitle,
		Detail: where + " It does not run again on its own, so the data is as it was. " + reasonSentence(failed.Reason),
		Retry:  true,
	}
}

// boundedClause is text for the page: trimmed, without a closing period, and
// at most updateFailedReasonLimit characters.
func boundedClause(text string) string {
	text = strings.TrimSuffix(strings.TrimSpace(text), ".")
	if runes := []rune(text); len(runes) > updateFailedReasonLimit {
		text = string(runes[:updateFailedReasonLimit-1]) + "…"
	}
	return text
}
