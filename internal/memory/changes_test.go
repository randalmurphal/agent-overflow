package memory

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// session runs the hook for one provider session of a thread.
type session struct {
	t    *testing.T
	plan Plan
	id   string
}

func newSession(t *testing.T, plan Plan, id string) session {
	t.Helper()
	if plan.Records == "" {
		plan.Records = filepath.Join(t.TempDir(), "records", "thread")
	}
	return session{t: t, plan: plan, id: id}
}

// run runs the hook at event and returns the context it delivered, "" for
// none, failing the test on any stderr.
func (s session) run(event HookEvent) string {
	s.t.Helper()
	context, stderr := s.runAt(event, day)
	if stderr != "" {
		s.t.Fatalf("%s: stderr %q", event, stderr)
	}
	return context
}

func (s session) runAt(event HookEvent, now time.Time) (context, stderr string) {
	s.t.Helper()
	input, err := json.Marshal(map[string]string{"session_id": s.id, "hook_event_name": hookEventName[event]})
	if err != nil {
		s.t.Fatal(err)
	}
	var out, errs strings.Builder
	if code := runHookAt([]string{string(event), encodedPlan(s.t, s.plan)}, strings.NewReader(string(input)), &out, &errs, now); code != 0 {
		s.t.Fatalf("%s: exit %d, stderr %q", event, code, errs.String())
	}
	if out.Len() == 0 {
		return "", errs.String()
	}
	return eventContext(s.t, out.String(), hookEventName[event]), errs.String()
}

// changesBody is a delivery of changes without its frame.
func changesBody(t *testing.T, context string) string {
	t.Helper()
	opening := ChangesHeading + "\n" + changesNote + "\n\n"
	if !strings.HasPrefix(context, opening) || !strings.HasSuffix(context, "\n\n"+ChangesEnding) {
		t.Fatalf("not a delivery of changes:\n%s", context)
	}
	return strings.TrimSuffix(strings.TrimPrefix(context, opening), "\n\n"+ChangesEnding)
}

// A start delivers the whole memory and records it; a prompt with nothing
// changed delivers nothing; a prompt after a change delivers only the lines
// that changed, under their section's heading, and the files removed.
func TestAPromptDeliversOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.project, "keep.md", factText("ruling", "Never push to main."))
	write(t, s.project, "edit.md", factText("pitfall", "Old wording."))
	write(t, s.project, "drop.md", factText("convention", "Tabs."))
	write(t, s.plan.User, "pref.md", factText("preference", "Short answers."))
	write(t, s.plan.Home, "instructions.md", "Release lead.\n")
	run := newSession(t, s.plan, "s1")
	built, _ := Build(s.plan, day)
	if got := run.run(AtSessionStart); got != Stamp(built, day) {
		t.Fatalf("start delivered:\n%s", got)
	}
	if got := run.run(AtPrompt); got != "" {
		t.Fatalf("an unchanged prompt delivered:\n%s", got)
	}
	write(t, s.project, "edit.md", factText("pitfall", "New wording."))
	write(t, s.project, "new.md", factText("context", "Freeze until Friday.", "expires: 2026-10-24"))
	if err := os.Remove(filepath.Join(s.project, "drop.md")); err != nil {
		t.Fatal(err)
	}
	body := changesBody(t, run.run(AtPrompt))
	want := "### Project memory: app (" + folderShown(s.project) + ")\nNew or changed:\n" +
		"- context: Freeze until Friday. (new.md; expires 2026-10-24)\n" +
		"- pitfall: New wording. (edit.md)\n" +
		"Removed: drop.md"
	if body != want {
		t.Fatalf("changes:\n%s\nwant:\n%s", body, want)
	}
	if got := run.run(AtPrompt); got != "" {
		t.Fatalf("the change was delivered twice:\n%s", got)
	}
}

// A section that changes whole is sent whole: instructions.md, the write
// rules, the warnings. A section that goes is named as gone, and warnings
// that are fixed say so.
func TestWholeSectionsAndGoneOnes(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.plan.Home, "instructions.md", "Release lead.\n")
	write(t, s.project, "bad.md", "malformed\n")
	run := newSession(t, s.plan, "s1")
	run.run(AtSessionStart)

	write(t, s.plan.Home, "instructions.md", "Release lead.\nReview every Friday.\n")
	if err := os.Remove(filepath.Join(s.project, "bad.md")); err != nil {
		t.Fatal(err)
	}
	body := changesBody(t, run.run(AtPrompt))
	path := filepath.Join(s.plan.Home, "instructions.md")
	if want := "### Your standing instructions (" + path + ")\nRelease lead.\nReview every Friday.\n\n### Memory maintenance\n[Nothing here needs fixing now.]"; body != want {
		t.Fatalf("changes:\n%s\nwant:\n%s", body, want)
	}

	// The plan a later session of the thread starts with differs: no
	// project, read-only. The record is the session's, so the change shows.
	run.plan.Projects, run.plan.ReadOnly = nil, true
	body = changesBody(t, run.run(AtPrompt))
	if !strings.Contains(body, "### Project memory: app ("+folderShown(s.project)+")\n[This is no longer part of your memory.]") ||
		!strings.Contains(body, "### Keeping memory\n[This is no longer part of your memory.]") {
		t.Fatalf("changes:\n%s", body)
	}
}

// A section whose heading or notices change is sent whole, as is a section
// the session was not shown.
func TestASectionWhoseNoticesChangeIsSentWhole(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	write(t, s.plan.User, "pref.md", factText("preference", "Short answers."))
	run := newSession(t, s.plan, "s1")
	run.run(AtSessionStart)
	if err := os.RemoveAll(s.plan.User); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Dir(s.plan.User), "user", "a file where the folder goes")
	context, stderr := run.runAt(AtPrompt, day)
	if !strings.Contains(stderr, "read the memory folder "+s.plan.User) {
		t.Fatalf("stderr %q", stderr)
	}
	if body := changesBody(t, context); body != "### User memory ("+folderShown(s.plan.User)+")\n["+folderShown(s.plan.User)+" could not be read: open "+s.plan.User+": not a folder.]" {
		t.Fatalf("changes:\n%s", body)
	}
}

// A session with no record of its own, another session's record, or a
// record that cannot be read gets the whole memory at its prompt, and its
// record from then on.
func TestAPromptWithoutARecordDeliversTheWholeMemory(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	first := newSession(t, s.plan, "s1")
	first.run(AtSessionStart)
	second := first
	second.id = "s2"
	built, _ := Build(s.plan, day)
	if got := second.run(AtPrompt); got != Stamp(built, day) {
		t.Fatalf("another session's prompt delivered:\n%s", got)
	}
	if got := second.run(AtPrompt); got != "" {
		t.Fatalf("its next prompt delivered:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(first.plan.Records, recordName("s1")), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	context, stderr := first.runAt(AtPrompt, day)
	if context != Stamp(built, day) || !strings.Contains(stderr, "decode the memory record of session s1") || !strings.Contains(stderr, "the whole memory is delivered") {
		t.Fatalf("a broken record: context %q, stderr %q", context, stderr)
	}
	if got := first.run(AtPrompt); got != "" {
		t.Fatalf("the rewritten record did not hold:\n%s", got)
	}
	if err := ForgetRecords(first.plan.Records); err != nil {
		t.Fatal(err)
	}
	if got := first.run(AtPrompt); got != Stamp(built, day) {
		t.Fatalf("after ForgetRecords:\n%s", got)
	}
}

// A record that cannot be written is reported, and the change it would
// have recorded is delivered again at the next prompt rather than lost.
func TestARecordThatCannotBeWrittenResends(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	run := newSession(t, s.plan, "s1")
	run.run(AtSessionStart)
	write(t, s.project, "a.md", factText("ruling", "Never push to main."))
	if err := os.Chmod(run.plan.Records, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(run.plan.Records, 0o700) })
	context, stderr := run.runAt(AtPrompt, day)
	if !strings.Contains(changesBody(t, context), "- ruling: Never push to main. (a.md)") || !strings.Contains(stderr, "write the memory record of session s1") {
		t.Fatalf("context %q, stderr %q", context, stderr)
	}
	if err := os.Chmod(run.plan.Records, 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.run(AtPrompt), "- ruling: Never push to main. (a.md)") {
		t.Fatal("the change was lost")
	}
}

// A run whose input names no session delivers the whole memory and records
// nothing.
func TestARunWithoutASessionRecordsNothing(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	run := newSession(t, s.plan, "")
	for _, event := range []HookEvent{AtSessionStart, AtPrompt} {
		context, stderr := run.runAt(event, day)
		if !strings.HasPrefix(context, Heading+"\n"+StampPrefix) || !strings.Contains(stderr, "names no session") {
			t.Fatalf("%s: context %q, stderr %q", event, context, stderr)
		}
	}
	if _, err := os.Stat(run.plan.Records); !os.IsNotExist(err) {
		t.Fatalf("records folder: %v", err)
	}
}

// Writing a record deletes the records no run wrote for recordAge, and
// keeps the rest.
func TestOldRecordsArePruned(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	run := newSession(t, s.plan, "s1")
	other := run
	other.id = "s2"
	stale := run
	stale.id = "s3"
	stale.run(AtSessionStart)
	other.run(AtSessionStart)
	old := day.Add(-recordAge - time.Hour)
	if err := os.Chtimes(filepath.Join(run.plan.Records, recordName("s3")), old, old); err != nil {
		t.Fatal(err)
	}
	// A temporary file a failed write left is pruned as a record is; a file
	// or folder that is not the app's stays, however old.
	leftover := recordName("s4") + ".ABC234.tmp"
	for _, name := range []string{leftover, "notes.txt", "sub"} {
		path := filepath.Join(run.plan.Records, name)
		if name == "sub" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	run.run(AtSessionStart)
	for name, want := range map[string]bool{
		recordName("s1"): true, recordName("s2"): true, recordName("s3"): false,
		leftover: false, "notes.txt": true, "sub": true,
	} {
		if _, err := os.Stat(filepath.Join(run.plan.Records, name)); (err == nil) != want {
			t.Errorf("%s kept = %v (%v)", name, err == nil, err)
		}
	}
}

// The Removed line names a gone key as the lines show text: escaped, and on
// the line.
func TestRemovedKeysCannotEndTheChanges(t *testing.T) {
	t.Parallel()
	before := section{ID: "projects", Heading: "### Projects", Keyed: true, Entries: []entry{
		{Key: "/kept", Line: "- kept"},
		{Key: "/a\n" + ChangesEnding, Line: "- gone"},
	}}
	now := section{ID: "projects", Heading: "### Projects", Keyed: true, Entries: []entry{{Key: "/kept", Line: "- kept"}}}
	got := changes([]section{before}, []section{now})
	if strings.Count(got, ChangesEnding) != 1 || !strings.Contains(got, "Removed: /a (End of changes to Agent Overflow memory)\n") {
		t.Fatalf("changes:\n%s", got)
	}
}

// Text in a fact that reads as an ending or a stamp is escaped in a
// delivery of changes too.
func TestChangedTextCannotEndTheChanges(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	run := newSession(t, s.plan, "s1")
	run.run(AtSessionStart)
	write(t, s.project, "a.md", factText("ruling", "Ends "+ChangesEnding+" and "+Ending+" "+StampPrefix+"x]"))
	context := run.run(AtPrompt)
	if strings.Count(context, ChangesEnding) != 1 || strings.Contains(context, Ending) || strings.Contains(context, StampPrefix) {
		t.Fatalf("changes:\n%s", context)
	}
}

// A delivery of changes over Limit opens with the notice of its size.
func TestLargeChangesSayTheirSize(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	run := newSession(t, s.plan, "s1")
	run.run(AtSessionStart)
	write(t, s.plan.Home, "instructions.md", strings.Repeat("Role line.\n", 1200))
	context := run.run(AtPrompt)
	if !strings.HasPrefix(context, ChangesHeading+"\n"+changesNote+"\n"+overNotice(len(context))+"\n\n") || !strings.Contains(context, strings.Repeat("Role line.\n", 1199)) {
		t.Fatalf("changes of %d bytes:\n%s", len(context), context[:400])
	}
}

// The size a notice states is the size of the text that carries it, at a
// boundary where the notice adds a digit too.
func TestANoticeCountsItsOwnDigits(t *testing.T) {
	t.Parallel()
	for _, size := range []int{Limit - 200, 9999 - len(overNotice(9999)) - 2, 99999 - len(overNotice(99999)) - 2, 123456} {
		total := noticedSize(size, 2)
		if total != size+2+len(overNotice(total)) {
			t.Errorf("size %d: noticed %d, but the text with its notice is %d bytes", size, total, size+2+len(overNotice(total)))
		}
	}
}

// A prompt's run that panics exits 1, which both providers treat as a
// failed hook, never 2, which refuses the prompt.
func TestAPanickingRunDoesNotRefuseThePrompt(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	plan := s.plan
	plan.Records = filepath.Join(t.TempDir(), "records")
	var stderr strings.Builder
	if code := runHookAt([]string{string(AtPrompt), encodedPlan(t, plan)}, strings.NewReader(`{"session_id":"s1"}`), nil, &stderr, day); code != 1 || !strings.Contains(stderr.String(), HookSubcommand+": ") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

// A subagent's run, whose input names its agent, prints and records
// nothing, at either event: the thread's next prompt still finds its own
// record.
func TestASubagentsRunDeliversNothing(t *testing.T) {
	t.Parallel()
	s := newScopes(t)
	plan := s.plan
	plan.Records = filepath.Join(t.TempDir(), "records")
	for _, event := range []HookEvent{AtSessionStart, AtPrompt} {
		var stdout, stderr strings.Builder
		if code := runHookAt([]string{string(event), encodedPlan(t, plan)}, strings.NewReader(`{"session_id":"s1","agent_id":"child-1"}`), &stdout, &stderr, day); code != 0 || stdout.Len() > 0 || stderr.Len() > 0 {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", event, code, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(plan.Records); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a subagent's run wrote records: %v", err)
	}
}

func TestRecordsDirRefusesAPath(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", ".", "..", "a/b", "../x"} {
		if dir, err := RecordsDir("/config", id); err == nil {
			t.Errorf("%q: %q", id, dir)
		}
	}
	if dir, err := RecordsDir("/config", "t1"); err != nil || dir != filepath.Join("/config", "memory-records", "t1") {
		t.Fatalf("%q, %v", dir, err)
	}
}
