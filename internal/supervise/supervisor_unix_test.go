//go:build !windows

package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The supervisor is a process that runs processes, so testing it any way other
// than by running one tests something else. The children here are SCRIPTED
// stand-ins: small shell scripts staged exactly where a real staged version
// would be, speaking the real protocol on the real inherited descriptors.
//
// Nothing in this file is session-capable and nothing reaches a provider. The
// environment handed to a child is PATH plus a HOME inside t.TempDir(), so a
// script cannot see the developer's real home even by accident, and none of
// them looks.

// fakeChildScript is the stand-in. The header answers the preflight; the body
// completes the handshake and then runs one behavior.
const fakeChildScript = `#!/bin/sh
VERSION='__VERSION__'
PROTO='__PROTO__'
OBS='__OBS__'
DB='__DB__'

if [ "$1" = '__PREFLIGHT__' ]; then
	printf '{"protocolVersion":%s,"version":"%s"}\n' "$PROTO" "$VERSION"
	exit 0
fi

note() { printf '%s\n' "$*" >> "$OBS/log"; }

serve_until_stopped() {
	trap 'note "stopped $VERSION"; exit 0' TERM INT
	while :; do
		# A backgrounded sleep plus wait, so the trap runs the instant the
		# signal lands rather than after a foreground sleep finishes.
		sleep 5 &
		wait $! 2>/dev/null
	done
}

IFS= read -r ACTIVATE <&3
printf '%s\n' "$ACTIVATE" >> "$OBS/activate"
printf '{"type":"hello","protocolVersion":%s,"version":"%s"}\n' "$PROTO" "$VERSION" >&4
note "hello $VERSION"

__BEHAVIOR__
`

// The behaviors, each a fragment the template runs after the handshake.
const (
	// behaviorServe is an ordinary backend: it does nothing until stopped.
	behaviorServe = `serve_until_stopped`

	// behaviorPrepare is a trial that succeeds. It records what it read from
	// the database before writing its own mark, which is what proves a restore
	// ran BEFORE it was started.
	behaviorPrepare = `cat "$DB" >> "$OBS/read" 2>/dev/null
printf 'trial' > "$DB"
printf '{"type":"prepared"}\n' >&4
note "prepared $VERSION"
IFS= read -r COMMIT <&3
printf '%s\n' "$COMMIT" >> "$OBS/commit"
note "committed $VERSION"
serve_until_stopped`

	// behaviorCrash is a trial that dies before reporting prepared, having
	// already written to the database.
	behaviorCrash = `printf 'trial' > "$DB"
note "crashing $VERSION"
exit 3`

	// behaviorHang is a trial that boots and never reports prepared.
	behaviorHang = `printf 'trial' > "$DB"
note "hanging $VERSION"
serve_until_stopped`
)

// behaviorRequestUpdate asks for a target, once per rig, and keeps serving.
// Once, because a version that re-requested on every restart would loop a
// rollback forever and prove nothing.
func behaviorRequestUpdate(target string) string {
	return `if [ ! -f "$OBS/requested" ]; then
	: > "$OBS/requested"
	printf '{"type":"request-update","targetVersion":"` + target + `"}\n' >&4
	note "requested ` + target + `"
	IFS= read -r ANSWER <&3
	printf '%s\n' "$ANSWER" >> "$OBS/answer"
	note "answered $VERSION"
fi
serve_until_stopped`
}

// rig is one supervised install under test.
type rig struct {
	t       *testing.T
	dataDir string
	obs     string
	home    string
	layout  Layout

	mu   sync.Mutex
	logs []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{
		t:       t,
		dataDir: filepath.Join(root, "data"),
		obs:     filepath.Join(root, "obs"),
		home:    filepath.Join(root, "home"),
	}
	for _, dir := range []string{r.dataDir, r.obs, r.home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	layout, err := NewLayout(r.dataDir)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	r.layout = layout
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("supervisor log:\n  %s", strings.Join(r.supervisorLog(), "\n  "))
		t.Logf("child log:\n  %s", strings.Join(r.lines("log"), "\n  "))
	})
	return r
}

// stage writes a scripted version into the versions directory.
func (r *rig) stage(version, behavior string) {
	r.t.Helper()
	r.stageProtocol(version, ProtocolVersion, behavior)
}

func (r *rig) stageProtocol(version string, protocol int, behavior string) {
	r.t.Helper()
	binary, err := r.layout.VersionBinary(version)
	if err != nil {
		r.t.Fatalf("VersionBinary: %v", err)
	}
	r.writeScript(binary, version, protocol, behavior)
}

// writeScript renders one scripted version to an arbitrary path. Separate from
// stage so a test can put a script somewhere the supervisor has to COPY it
// from, which is the fresh-install case.
func (r *rig) writeScript(path, version string, protocol int, behavior string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	script := strings.NewReplacer(
		"__VERSION__", version,
		"__PROTO__", strconv.Itoa(protocol),
		"__OBS__", r.obs,
		"__DB__", filepath.Join(r.dataDir, DatabaseFiles()[0]),
		"__PREFLIGHT__", PreflightSubcommand,
		"__BEHAVIOR__", behavior,
	).Replace(fakeChildScript)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		r.t.Fatalf("write %s: %v", path, err)
	}
}

// adopt records a version as the active one, the way a fresh install would.
func (r *rig) adopt(version string) {
	r.t.Helper()
	state, err := Adopt(version)
	if err != nil {
		r.t.Fatalf("Adopt: %v", err)
	}
	if err := SaveState(r.layout, state); err != nil {
		r.t.Fatalf("SaveState: %v", err)
	}
}

func (r *rig) config() Config {
	return Config{
		DataDir:        r.dataDir,
		SelfExecutable: filepath.Join(r.obs, "supervisor-binary"),
		SelfVersion:    "0.0.0-supervisor",
		ChildArgs:      []string{"serve"},
		// PATH for `sleep`, and a HOME that is not the developer's. Nothing
		// else: a scripted child has no business resolving anything.
		Env:           []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home},
		Log:           r.log,
		TrialBudget:   10 * time.Second,
		ResponseGrace: 20 * time.Millisecond,
		StopTimeout:   5 * time.Second,
	}
}

func (r *rig) log(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *rig) supervisorLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// run starts a supervisor and returns a stop func plus its result channel.
// Every test stops the supervisor and reads the result, so no goroutine
// outlives the test that started it.
func (r *rig) run(config Config) (stop func(), done <-chan error) {
	r.t.Helper()
	supervisor, err := New(config)
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 1)
	go func() { results <- supervisor.Run(ctx) }()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
	}, results
}

// runUntil starts a supervisor, waits for a condition in the child log, then
// stops it and returns its result.
func (r *rig) runUntil(config Config, want string, count int) error {
	r.t.Helper()
	stop, done := r.run(config)
	defer stop()
	r.waitForLog(want, count)
	stop()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		r.t.Fatal("the supervisor did not return after its context was cancelled")
		return nil
	}
}

func (r *rig) lines(name string) []string {
	data, err := os.ReadFile(filepath.Join(r.obs, name))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// waitForLog blocks until the child log holds at least count lines equal to
// want. A missing line is a test failure, never a silent pass.
func (r *rig) waitForLog(want string, count int) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		seen := 0
		for _, line := range r.lines("log") {
			if line == want {
				seen++
			}
		}
		if seen >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatalf("waited for %d x %q in the child log; saw %v", count, want, r.lines("log"))
}

func (r *rig) state() State {
	r.t.Helper()
	state, found, err := LoadState(r.layout)
	if err != nil || !found {
		r.t.Fatalf("LoadState = (found %t, %v)", found, err)
	}
	return state
}

func (r *rig) database() string {
	r.t.Helper()
	return readFile(r.t, filepath.Join(r.dataDir, DatabaseFiles()[0]))
}

// A fresh install has no state and no versions. The supervisor stages ITSELF,
// records that as active, and runs it — so "previous" names an immutable
// directory from the very first boot.
func TestAFreshInstallAdoptsTheSupervisorsOwnBinary(t *testing.T) {
	rig := newRig(t)
	config := rig.config()
	// The supervisor's own executable, sitting where a service manager would
	// have started it from: outside the versions directory entirely.
	rig.writeScript(config.SelfExecutable, config.SelfVersion, ProtocolVersion, behaviorServe)

	if err := rig.runUntil(config, "hello 0.0.0-supervisor", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := rig.state()
	if state.ActiveVersion != config.SelfVersion || state.Update != nil {
		t.Fatalf("state = %+v, want %s active with no record", state, config.SelfVersion)
	}
	staged := filepath.Join(rig.layout.VersionsDir(), config.SelfVersion, BinaryName)
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("the supervisor did not stage a copy of itself: %v", err)
	}
	if readFile(t, staged) != readFile(t, config.SelfExecutable) {
		t.Error("the staged copy is not the supervisor's own bytes")
	}
}

// The happy path, end to end: a running backend asks, the supervisor accepts,
// snapshots, trials, and commits, and the trial's database work survives.
func TestAnUpdateCommitsAndKeepsTheTrialsWork(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stage("2.0.0", behaviorPrepare)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	if err := rig.runUntil(rig.config(), "committed 2.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := rig.state()
	if state.Update == nil || state.Update.State != UpdateCommitted {
		t.Fatalf("state = %+v, want a committed record", state.Update)
	}
	if state.Update.From != "1.0.0" || state.Update.To != "2.0.0" {
		t.Fatalf("record = %+v, want 1.0.0 -> 2.0.0", state.Update)
	}
	// The next restart selects the target ordinarily, with no trial.
	selection, err := state.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Version != "2.0.0" || selection.Trial {
		t.Fatalf("selection = %+v, want 2.0.0 with no trial", selection)
	}
	// The committing child published the outcome from its commit frame, so
	// the record is already reported and the next boot announces nothing.
	if !state.Update.Reported {
		t.Error("a committed record was saved unreported; the next boot would announce it again")
	}
	if selection.Outcome != "" {
		t.Errorf("selection after commit carries outcome %q, want none", selection.Outcome)
	}

	// The two frames the client correlation in the next wave rides on: the id
	// the requester was told, and the id the trial was committed under.
	answer := rig.lines("answer")
	commit := rig.lines("commit")
	if len(answer) != 1 || len(commit) != 1 {
		t.Fatalf("answer=%v commit=%v, want one of each", answer, commit)
	}
	if !strings.Contains(answer[0], `"type":"`+MsgUpdateAccepted+`"`) {
		t.Fatalf("the requester was answered %q, want an %s", answer[0], MsgUpdateAccepted)
	}
	for _, frame := range []string{answer[0], commit[0]} {
		if !strings.Contains(frame, state.Update.ID) {
			t.Errorf("frame %q does not carry update id %s", frame, state.Update.ID)
		}
	}

	// A commit keeps the trial's database and drops the snapshot.
	if got := rig.database(); got != "trial" {
		t.Errorf("database = %q, want the trial's own work", got)
	}
	if !absent(t, rig.layout.SnapshotDir()) {
		t.Error("the snapshot survived a commit")
	}
	if !absent(t, rig.layout.MarkerPath()) {
		t.Error("a commit left a restore marker")
	}
	// The trial read the pre-update database, which is what a snapshot taken
	// while nothing held the file means.
	if read := rig.lines("read"); len(read) != 1 || read[0] != "before agent-overflow.db" {
		t.Errorf("the trial read %v, want the pre-update database", read)
	}
}

// A trial that dies before reporting prepared: the snapshot goes back over its
// work and the previous version restarts.
func TestATrialThatCrashesIsRolledBack(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stage("2.0.0", behaviorCrash)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	// Two hellos from 1.0.0: the one that asked, and the one the rollback
	// restarted.
	stop, done := rig.run(rig.config())
	defer stop()
	rig.waitForLog("hello 1.0.0", 2)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := rig.state()
	if state.Update == nil || state.Update.State != UpdateRolledBack {
		t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	if !strings.Contains(state.Update.Reason, "exited before reporting prepared") {
		t.Errorf("reason = %q, which does not say the trial exited", state.Update.Reason)
	}
	// The exit STATUS, not just the fact of an exit. The pipe closing and the
	// process exiting arrive on two channels in whichever order the scheduler
	// picks, and settling on the first made this reason a coin flip.
	if !strings.Contains(state.Update.Reason, "3") {
		t.Errorf("reason = %q, which does not carry the trial's exit status", state.Update.Reason)
	}
	selection, err := state.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Version != "1.0.0" || selection.Trial {
		t.Fatalf("selection = %+v, want 1.0.0 with no trial", selection)
	}
	if got := rig.database(); got != "before agent-overflow.db" {
		t.Errorf("database = %q, want the pre-update contents restored", got)
	}
	if !absent(t, rig.layout.MarkerPath()) {
		t.Error("the restore marker survived a completed rollback")
	}
	if !absent(t, rig.layout.SnapshotDir()) {
		t.Error("the snapshot survived a completed rollback")
	}
}

// A trial that boots and never reports prepared is rolled back at the budget.
func TestATrialThatNeverPreparesIsRolledBackAtTheBudget(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stage("2.0.0", behaviorHang)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	config := rig.config()
	config.TrialBudget = 400 * time.Millisecond

	stop, done := rig.run(config)
	defer stop()
	rig.waitForLog("hello 1.0.0", 2)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := rig.state()
	if state.Update == nil || state.Update.State != UpdateRolledBack {
		t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	if !strings.Contains(state.Update.Reason, "did not report prepared within") {
		t.Errorf("reason = %q, which does not name the budget", state.Update.Reason)
	}
	if got := rig.database(); got != "before agent-overflow.db" {
		t.Errorf("database = %q, want the pre-update contents restored", got)
	}
	// The hung trial was stopped, not left running beside its replacement.
	if stops := countLines(rig.lines("log"), "stopped 2.0.0"); stops != 1 {
		t.Errorf("the trial was stopped %d times, want once", stops)
	}
}

// The requirement the whole durable-state design exists for: a supervisor
// killed at any point recovers from the state file and the marker alone.
//
// Three supervisors run over one install. The first two are killed while the
// trial is in flight; the third finds the attempt limit reached and rolls back
// without starting a fourth.
func TestASupervisorKilledMidTrialResumesAndEventuallyRollsBack(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stage("2.0.0", behaviorHang)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	// First supervisor: accepts the update, snapshots, starts the trial, and
	// is killed while it hangs.
	if err := rig.runUntil(rig.config(), "hanging 2.0.0", 1); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	state := rig.state()
	if state.Update == nil || state.Update.State != UpdatePending {
		t.Fatalf("state = %+v, want the update still pending", state.Update)
	}
	if state.Update.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.Update.Attempts)
	}
	if absent(t, rig.layout.SnapshotDir()) {
		t.Fatal("the snapshot was discarded by a supervisor that was killed mid-trial")
	}
	if got := rig.database(); got != "trial" {
		t.Fatalf("database = %q, want the trial's own work still in place", got)
	}

	// Second supervisor: state alone tells it the trial is unfinished, and it
	// tries again. Nothing else on disk changed.
	if err := rig.runUntil(rig.config(), "hanging 2.0.0", 2); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := rig.state().Update.Attempts; got != TrialAttemptLimit {
		t.Fatalf("attempts = %d, want the limit %d", got, TrialAttemptLimit)
	}

	// Third supervisor: the limit is reached, so it rolls back rather than
	// starting a trial that has twice failed to finish.
	stop, done := rig.run(rig.config())
	defer stop()
	rig.waitForLog("hello 1.0.0", 2)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("third Run: %v", err)
	}

	state = rig.state()
	if state.Update.State != UpdateRolledBack {
		t.Fatalf("state = %+v, want a rolled-back record", state.Update)
	}
	if !strings.Contains(state.Update.Reason, "interrupted") {
		t.Errorf("reason = %q, which does not say the trial was interrupted", state.Update.Reason)
	}
	if got := rig.database(); got != "before agent-overflow.db" {
		t.Errorf("database = %q, want the pre-update contents restored", got)
	}
	// Exactly two trials were started across three supervisors.
	if hangs := countLines(rig.lines("log"), "hanging 2.0.0"); hangs != TrialAttemptLimit {
		t.Errorf("the trial started %d times, want %d", hangs, TrialAttemptLimit)
	}
}

// A restore interrupted mid-copy is finished BEFORE any version is selected,
// which is why the trial that then runs reads the snapshot's database and not
// the half-restored one.
func TestAMarkedRestoreIsFinishedBeforeAnythingIsSpawned(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorServe)
	rig.stage("2.0.0", behaviorPrepare)
	writeDatabase(t, rig.dataDir, "before")

	// Exactly what a supervisor killed one instruction into a restore leaves:
	// a pending update, a snapshot, a marker, and a database that is neither.
	if _, err := TakeSnapshot(rig.layout, rig.dataDir, time.Unix(0, 0)); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	writeFile(t, filepath.Join(rig.dataDir, DatabaseFiles()[0]), "half-restored")
	writeFile(t, rig.layout.MarkerPath(),
		`{"updateId":"u1","dataDir":`+quote(rig.dataDir)+`,"reason":"killed","writtenAtMs":1}`)
	state := State{Schema: StateSchema, ActiveVersion: "1.0.0", Update: &UpdateRecord{
		ID: "u1", State: UpdatePending, From: "1.0.0", To: "2.0.0",
	}}
	if err := SaveState(rig.layout, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := rig.runUntil(rig.config(), "committed 2.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The proof the restore ran first: the trial read the SNAPSHOT's database,
	// not the half-restored bytes that were on disk when the supervisor booted.
	if read := rig.lines("read"); len(read) != 1 || read[0] != "before agent-overflow.db" {
		t.Fatalf("the trial read %v, want the restored database", read)
	}
	if !absent(t, rig.layout.MarkerPath()) {
		t.Error("the marker survived the resumed restore")
	}
	if got := rig.state().Update.State; got != UpdateCommitted {
		t.Errorf("state = %q, want committed", got)
	}
}

// Fail closed: a state file the supervisor cannot read is an error and an
// exit, never a guess about which version to run.
func TestAnInvalidStateFileStartsNothing(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorServe)
	writeFile(t, rig.layout.StatePath(), `{"schema":99,"activeVersion":"1.0.0"}`)

	stop, done := rig.run(rig.config())
	defer stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run accepted a state file it cannot read")
		}
		if !strings.Contains(err.Error(), "schema") {
			t.Errorf("error = %v, which does not name the schema", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return on an invalid state file")
	}
	if lines := rig.lines("log"); len(lines) != 0 {
		t.Errorf("a version was started despite an unreadable state file: %v", lines)
	}
}

// A target that is not staged is refused with a reason, and nothing durable
// moves: a pending record naming a version that cannot run is a rollback the
// operator did not need to pay for.
func TestAnUnstagedTargetIsRefusedAndTheStateIsUntouched(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("9.9.9"))
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	if err := rig.runUntil(rig.config(), "answered 1.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	answer := rig.lines("answer")
	if len(answer) != 1 || !strings.Contains(answer[0], `"type":"`+MsgUpdateRefused+`"`) {
		t.Fatalf("answer = %v, want an %s", answer, MsgUpdateRefused)
	}
	if state := rig.state(); state.Update != nil {
		t.Fatalf("a refused update wrote a record: %+v", state.Update)
	}
	if got := rig.database(); got != "before agent-overflow.db" {
		t.Errorf("database = %q, want it untouched", got)
	}
	if !absent(t, rig.layout.SnapshotDir()) {
		t.Error("a refused update took a snapshot")
	}
}

// A target speaking a newer protocol than this supervisor is refused at the
// preflight, before anything is written down, and the refusal names the remedy.
func TestATargetSpeakingANewerProtocolIsRefused(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stageProtocol("2.0.0", ProtocolVersion+1, behaviorPrepare)
	rig.adopt("1.0.0")
	writeDatabase(t, rig.dataDir, "before")

	if err := rig.runUntil(rig.config(), "answered 1.0.0", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	answer := rig.lines("answer")
	if len(answer) != 1 || !strings.Contains(answer[0], `"type":"`+MsgUpdateRefused+`"`) {
		t.Fatalf("answer = %v, want an %s", answer, MsgUpdateRefused)
	}
	if !strings.Contains(answer[0], "service update") {
		t.Errorf("the refusal %q does not name the remedy", answer[0])
	}
	if state := rig.state(); state.Update != nil {
		t.Fatalf("a refused update wrote a record: %+v", state.Update)
	}
}

// An update whose snapshot cannot be taken never reached a trial, so it
// settles FAILED rather than rolled-back, and the previous version restarts.
func TestAnUpdateThatCannotBeSnapshottedFailsAndRestartsThePrevious(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", behaviorRequestUpdate("2.0.0"))
	rig.stage("2.0.0", behaviorPrepare)
	rig.adopt("1.0.0")
	// No database at all: nothing to snapshot, so nothing to roll back to.

	stop, done := rig.run(rig.config())
	defer stop()
	rig.waitForLog("hello 1.0.0", 2)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := rig.state()
	if state.Update == nil || state.Update.State != UpdateFailed {
		t.Fatalf("state = %+v, want a failed record", state.Update)
	}
	if !strings.Contains(state.Update.Reason, "snapshot") {
		t.Errorf("reason = %q, which does not name the snapshot", state.Update.Reason)
	}
	// The trial never ran, so 2.0.0 never said hello.
	if hellos := countLines(rig.lines("log"), "hello 2.0.0"); hellos != 0 {
		t.Errorf("the trial started %d times despite no snapshot", hellos)
	}
}

// A clean child exit is the operator stopping the backend, and it ends the
// supervisor with the child's own status so `Restart=on-failure` keeps meaning
// what it meant.
func TestTheSupervisorMirrorsItsChildsExit(t *testing.T) {
	rig := newRig(t)
	rig.stage("1.0.0", `note "exiting 1.0.0"
exit 7`)
	rig.adopt("1.0.0")

	stop, done := rig.run(rig.config())
	defer stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a child that exited 7")
		}
		if !strings.Contains(err.Error(), "7") {
			t.Errorf("error = %v, which does not carry the child's status", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return when its child exited")
	}
}

// The child half of the channel: absent marker, no channel, no error. A
// supervisor is optional forever.
func TestOpenChildChannelIsAbsentWithoutTheMarker(t *testing.T) {
	conn, err := OpenChildChannel(
		func(string) (string, bool) { return "", false },
		func(string) error { t.Fatal("the marker was unset when there was none"); return nil },
	)
	if err != nil || conn != nil {
		t.Fatalf("OpenChildChannel = (%v, %v), want (nil, nil)", conn, err)
	}
}

// And with one: the descriptors are opened, and the marker is cleared BEFORE
// anything can fail, so nothing the child spawns inherits a claim to a channel
// it does not hold.
func TestOpenChildChannelOpensThePipesAndClearsTheMarker(t *testing.T) {
	read, writeToChild, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer writeToChild.Close()
	readFromChild, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer readFromChild.Close()

	cleared := false
	conn, err := OpenChildChannel(
		func(string) (string, bool) {
			return strconv.Itoa(int(read.Fd())) + "," + strconv.Itoa(int(write.Fd())), true
		},
		func(string) error { cleared = true; return nil },
	)
	if err != nil {
		t.Fatalf("OpenChildChannel: %v", err)
	}
	if conn == nil {
		t.Fatal("OpenChildChannel returned no channel for a present marker")
	}
	if !cleared {
		t.Error("the marker was not cleared")
	}

	if err := conn.Send(Message{Type: MsgHello, ProtocolVersion: ProtocolVersion, Version: "1.0.0"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	other := NewConn(readFromChild, writeToChild, nil)
	got, err := other.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got.Type != MsgHello || got.Version != "1.0.0" {
		t.Fatalf("received %+v", got)
	}
}

// A marker pointing at descriptors that are not pipes is a broken spawn, and
// inheriting somebody else's fd 3 as a control channel is worth failing on.
func TestOpenChildChannelRefusesDescriptorsThatAreNotPipes(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer file.Close()
	fd := strconv.Itoa(int(file.Fd()))
	if _, err := OpenChildChannel(
		func(string) (string, bool) { return fd + "," + fd, true },
		func(string) error { return nil },
	); err == nil {
		t.Fatal("OpenChildChannel accepted a regular file as a control channel")
	}
	for _, value := range []string{"3", "x,4", "3,y", "  "} {
		conn, err := OpenChildChannel(
			func(string) (string, bool) { return value, true },
			func(string) error { return nil },
		)
		if value == "  " {
			if err != nil || conn != nil {
				t.Errorf("a blank marker = (%v, %v), want (nil, nil)", conn, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("OpenChildChannel accepted marker %q", value)
		}
	}
}

func countLines(lines []string, want string) int {
	n := 0
	for _, line := range lines {
		if line == want {
			n++
		}
	}
	return n
}
