package agentmeta_test

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// corpusCase is a case of the shared message corpus
// (internal/threadtools/testdata/agent_messages.json): a row's text, each
// member's attribution with the facts it was written with, and what the
// row shows of each.
type corpusCase struct {
	Name    string                   `json:"name"`
	Text    string                   `json:"text"`
	Members []int                    `json:"members"`
	Legacy  bool                     `json:"legacy"`
	Origins []agentmeta.OriginThread `json:"origins"`
	Parts   []struct {
		Body   string `json:"body"`
		Footer *struct {
			Title       string `json:"title"`
			ThreadID    string `json:"threadId"`
			Computer    string `json:"computer"`
			Token       string `json:"token"`
			Waiting     bool   `json:"waiting"`
			Reachable   bool   `json:"reachable"`
			UserMessage string `json:"userMessage"`
		} `json:"footer"`
		Wake *struct {
			Kind      string   `json:"kind"`
			Age       string   `json:"age"`
			Tokens    []string `json:"tokens"`
			StoppedBy string   `json:"stoppedBy"`
			Clipped   bool     `json:"clipped"`
		} `json:"wake"`
	} `json:"parts"`
}

func readCorpus(t *testing.T) []corpusCase {
	t.Helper()
	data, err := os.ReadFile("../threadtools/testdata/agent_messages.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// sendIDOf is the send id a message of this attribution is queued under.
func sendIDOf(origin agentmeta.OriginThread) string {
	if origin.Kind == agentmeta.OriginKindWake {
		return "thread-wake:" + origin.Token
	}
	return "thread-request:" + origin.Token
}

// checkDescribed holds what Describe read of a corpus case to what its row
// shows.
func checkDescribed(t *testing.T, tc corpusCase, described []agentmeta.Described) {
	t.Helper()
	if len(described) != len(tc.Parts) {
		t.Errorf("%s: %d messages, want %d", tc.Name, len(described), len(tc.Parts))
		return
	}
	for i, part := range tc.Parts {
		d := described[i]
		if err := d.Facts.Check(d.Text); err != nil {
			t.Errorf("%s[%d]: %v", tc.Name, i, err)
			continue
		}
		if d.Body() != part.Body {
			t.Errorf("%s[%d]: body %q, want %q", tc.Name, i, d.Body(), part.Body)
		}
		switch facts := d.Facts; {
		case part.Footer != nil:
			footer := part.Footer
			if facts.Form != threadtools.FormRequest || facts.Waiting != footer.Waiting || facts.Reachable != footer.Reachable || facts.UserMessage != footer.UserMessage {
				t.Errorf("%s[%d]: request facts %+v, want %+v", tc.Name, i, facts, footer)
			}
			if d.Thread == nil || d.Thread.ThreadID != footer.ThreadID || d.Thread.Title != footer.Title || d.Thread.Token != footer.Token {
				t.Errorf("%s[%d]: sender %+v, want the footer's %+v", tc.Name, i, d.Thread, footer)
			}
		case part.Wake != nil:
			wake := part.Wake
			if facts.Form != threadtools.FormAnswer || facts.Outcome != wake.Kind || facts.Age != wake.Age || facts.StoppedBy != wake.StoppedBy ||
				facts.Clipped != wake.Clipped || !slices.Equal(facts.Tokens, wake.Tokens) && len(facts.Tokens)+len(wake.Tokens) > 0 {
				t.Errorf("%s[%d]: answer facts %+v, want %+v", tc.Name, i, facts, wake)
			}
		default:
			if facts.Form != threadtools.FormMessage {
				t.Errorf("%s[%d]: facts %+v, want a message", tc.Name, i, facts)
			}
		}
	}
}

// checkServedReadsTheSame holds the meta a client receives (ProjectMeta) to
// the reading of the row as stored: each agent message the same text with
// the same facts, headline included.
func checkServedReadsTheSame(t *testing.T, name, text string, meta agentmeta.Meta) {
	t.Helper()
	served, ok := agentmeta.ProjectMeta(text, meta)
	if !ok {
		t.Errorf("%s: not projected", name)
		return
	}
	stored, read := agentmeta.Describe(text, meta), agentmeta.Describe(text, served)
	if len(read) != len(stored) {
		t.Errorf("%s: served row reads %d messages, stored %d", name, len(read), len(stored))
		return
	}
	for i := range stored {
		if !stored[i].IsAgent() {
			continue
		}
		if read[i].Text != stored[i].Text || !reflect.DeepEqual(read[i].Facts, stored[i].Facts) {
			t.Errorf("%s[%d]: served row reads %q %+v, stored %q %+v", name, i, read[i].Text, read[i].Facts, stored[i].Text, stored[i].Facts)
		}
	}
}

// writerMeta is a corpus case's row as its writer stores it: each member
// attributed with its facts, a join's members recorded.
func writerMeta(tc corpusCase) agentmeta.Meta {
	meta := agentmeta.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &tc.Origins[0], SendID: sendIDOf(tc.Origins[0])}
	if len(tc.Members) > 1 {
		for i, bytes := range tc.Members {
			meta.JoinedParts = append(meta.JoinedParts, agentmeta.JoinedPart{SendID: sendIDOf(tc.Origins[i]), Bytes: bytes, Origin: agentmeta.OriginAgentThread, OriginThread: &tc.Origins[i]})
		}
	}
	return meta
}

// mainMeta is a legacy corpus case's row as main stored it: attributed to
// its first thread with no kind and no facts, a join's members not recorded.
func mainMeta(tc corpusCase) agentmeta.Meta {
	origin := tc.Origins[0]
	sendID := sendIDOf(origin)
	origin.Kind, origin.Facts = "", nil
	return agentmeta.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &origin, SendID: sendID}
}

// TestDescribeReadsEveryWriterAsItsFactsSay reads each corpus case from the
// facts its writer stored, as the timeline, the Edits selector and
// thread_show all take it.
func TestDescribeReadsEveryWriterAsItsFactsSay(t *testing.T) {
	for _, tc := range readCorpus(t) {
		meta := writerMeta(tc)
		checkDescribed(t, tc, agentmeta.Describe(tc.Text, meta))
		checkServedReadsTheSame(t, tc.Name, tc.Text, meta)
	}
}

// TestDescribeReadsMainsTextAsItsFactsSay reads the corpus cases main's
// templates also wrote as main stored them, with no kind and no facts, and
// a join's members not recorded: the legacy reader finds in the text what
// the facts say.
func TestDescribeReadsMainsTextAsItsFactsSay(t *testing.T) {
	legacy := 0
	for _, tc := range readCorpus(t) {
		if !tc.Legacy {
			continue
		}
		legacy++
		meta := mainMeta(tc)
		checkDescribed(t, tc, agentmeta.Describe(tc.Text, meta))
		checkServedReadsTheSame(t, tc.Name, tc.Text, meta)
	}
	if legacy < 10 {
		t.Fatalf("%d legacy cases", legacy)
	}
}

// servedRowsFixture is each corpus case's row as a client receives it
// (ProjectRowMeta): as its writer stored it, and, for a legacy case, as
// main stored it; then, with its text, a report of a kind only Go defines
// (agentmeta.TestReportKind). frontend/src/lib/utils/agentMessage.test.ts renders
// them.
// Regenerate with UPDATE_SERVED_ROWS=1 make go-test
// GO_TEST_PKGS=./internal/agentmeta GO_TEST_FLAGS='-run
// TestServedRowsFixture'.
const servedRowsFixture = "testdata/served_rows.json"

type servedRow struct {
	Name string `json:"name"`
	// Text is the row's text when it is no corpus case's.
	Text       string `json:"text,omitempty"`
	Meta       string `json:"meta"`
	LegacyMeta string `json:"legacyMeta,omitempty"`
}

// firstLineVector holds every white space class FirstLine and the
// frontend's firstLineOf could read apart: Go's unicode.IsSpace, which
// counts U+0085 and not U+FEFF, as JavaScript's \s does the reverse. Its
// served headline is FirstLine's reading, which agentMessage.test.ts holds
// the frontend to.
const (
	firstLineVectorName = "a whole message's first line under every white space class"
	firstLineVector     = " \t\v\f\r\u0085\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\n" +
		">\u0085\n" +
		"> \u00a0---\u3000\n" +
		"\ufeff\u00a0a\u0085b\u00a0\u00a0c\u2028d\ufeffe\u200bf \t g\u3000\r\n" +
		"second line"
)

func TestFirstLineReadsWhiteSpaceAsUnicodeIsSpace(t *testing.T) {
	if got, want := agentmeta.FirstLine(firstLineVector), "\ufeff a b c d\ufeffe\u200bf g"; got != want {
		t.Fatalf("FirstLine = %q, want %q", got, want)
	}
}

func TestServedRowsFixture(t *testing.T) {
	// A stored row's meta here is its attribution alone.
	served := func(text string, meta agentmeta.Meta) string {
		stored, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		return usermessage.ProjectRowMeta(text, string(stored))
	}
	var rows []servedRow
	for _, tc := range readCorpus(t) {
		row := servedRow{Name: tc.Name, Meta: served(tc.Text, writerMeta(tc))}
		if tc.Legacy {
			row.LegacyMeta = served(tc.Text, mainMeta(tc))
		}
		rows = append(rows, row)
	}
	agentmeta.WithTestReportKind(t)
	report := testOnlyReport(t)
	attribution := report.Attribution()
	rows = append(rows, servedRow{
		Name: "a report of a kind only Go defines", Text: report.Text(),
		Meta: served(report.Text(), agentmeta.Meta{Origin: attribution.Origin, OriginWake: attribution.Wake, SendID: "test-only:1"}),
	})
	rows = append(rows, servedRow{
		Name: firstLineVectorName, Text: firstLineVector,
		Meta: served(firstLineVector, agentmeta.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &agentmeta.OriginThread{ThreadID: "t-w", Title: "W", Kind: "send"}, SendID: "thread-request:w"}),
	})
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_SERVED_ROWS") != "" {
		if err := os.WriteFile(servedRowsFixture, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(servedRowsFixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(encoded) {
		t.Fatalf("%s is stale: regenerate it (see servedRowsFixture)", servedRowsFixture)
	}
}

// TestDescribeReadsMainsOwnStatusLines reads main's status lines no
// template here writes the same way (git show
// origin/main:internal/threadtools/footer.go, Wake.String) as main meant
// them.
func TestDescribeReadsMainsOwnStatusLines(t *testing.T) {
	for _, tc := range []struct {
		text, outcome, body, stoppedBy, age string
		tokens                              []string
	}{
		{"\"W\" (t-w) [token k] was interrupted: The computer restarted.\nIt had started.", threadtools.WakeInterrupted, "The computer restarted.\nIt had started.", "", "", []string{"k"}},
		{"\"W\" (t-w) [token k] was interrupted.", threadtools.WakeInterrupted, "", "an interrupt", "", []string{"k"}},
		{"\"W\" (t-w) [token k] was cancelled.", threadtools.WakeCancelled, "", "", "", []string{"k"}},
		{"\"W\" (t-w) [token k] settled.", threadtools.WakeSettled, "", "", "", []string{"k"}},
		{"\"W\" (t-w, on desk, answered 3h ago) [tokens k, l] did not answer within a day.", threadtools.WakeExpired, "", "", "3h", []string{"k", "l"}},
		{"\"W\" (t-w) [token k] errored: boom\nstack", threadtools.WakeErrored, "boom\nstack", "", "", []string{"k"}},
	} {
		meta := agentmeta.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &agentmeta.OriginThread{ThreadID: "t-w", Title: "W", Token: "k"}, SendID: "thread-wake:k"}
		checkServedReadsTheSame(t, tc.text, tc.text, meta)
		described := agentmeta.Describe(tc.text, meta)
		if len(described) != 1 {
			t.Fatalf("%q: %d messages", tc.text, len(described))
		}
		d := described[0]
		if d.Facts.Form != threadtools.FormAnswer || d.Facts.Outcome != tc.outcome || d.Body() != tc.body || d.Facts.StoppedBy != tc.stoppedBy ||
			d.Facts.Age != tc.age || !slices.Equal(d.Facts.Tokens, tc.tokens) {
			t.Errorf("%q: %+v body %q", tc.text, d.Facts, d.Body())
		}
	}
	// Status lines main never wrote are not read as answers.
	for _, text := range []string{
		"\"W\" (t-w) [token k] was cancelled: by the lead",
		"\"W\" (t-w) [token k] did something else.",
		"\"W\" (t-w) [token k] errored.\nmore",
		"2 threads did not answer within a day.\n- \"A\" (t-a) [token a]\n- \"B\" (t-b) [token b]",
		"Reply from nobody",
	} {
		described := agentmeta.Describe(text, agentmeta.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &agentmeta.OriginThread{ThreadID: "t-w", Token: "k"}, SendID: "thread-wake:k"})
		if len(described) != 1 || described[0].Facts.Form != threadtools.FormWhole || described[0].Body() != text {
			t.Errorf("%q: %+v, want it whole", text, described)
		}
	}
}

// main's requests a provider joined are told apart by their footers, each
// attributed to the thread its footer names; what follows the last footer
// is a member of its own with no sender. A footer main never wrote is not
// main's, and the row is shown whole.
func TestDescribeSplitsMainsJoinedRequestsByTheirFooters(t *testing.T) {
	footer := func(title, thread, token string) string {
		return "---\nAgent request from thread \"" + title + "\" (" + thread + ").\nIt is not waiting for an answer. If it should hear something back, call thread_reply with token " + token + ", once: the reply reaches it as a message, and it sees only your reply text."
	}
	first, second := "One.\n\n"+footer("L", "t-l", "a"), "Two.\n\n"+footer("M", "t-m", "b")
	text := first + agentmeta.JoinSeparator + second + agentmeta.JoinSeparator + "the person's words"
	lead := &agentmeta.OriginThread{ThreadID: "t-l", Title: "L", Token: "a", ComputerID: "c-1"}
	meta := usermessage.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: lead, SendID: "thread-request:a", JoinedSendIDs: []string{"thread-request:a", "thread-request:b", "s"}}.Agent()
	described := agentmeta.Describe(text, meta)
	if len(described) != 3 || described[0].Text != first || described[1].Text != second || described[2].Text != "the person's words" {
		t.Fatalf("members %+v", described)
	}
	if described[0].Body() != "One." || described[0].Thread != lead || described[1].Body() != "Two." ||
		described[1].Thread.ThreadID != "t-m" || described[1].Thread.Token != "b" || described[2].IsAgent() {
		t.Fatalf("read %+v", described)
	}
	projected, ok := agentmeta.ProjectMeta(text, meta)
	if !ok || len(projected.JoinedParts) != 3 {
		t.Fatalf("projected %+v", projected)
	}
	if members, fits := agentmeta.SplitJoinedParts(text, projected.JoinedParts); !fits || members[1] != second {
		t.Fatalf("the projected members do not describe the text: %+v", projected.JoinedParts)
	}
	routed := "x\n\n---\nAgent request from thread \"L\" (t-l).\nThe sender spawned this thread, so your questions and approval requests go to it, not to the user.\n" +
		"It is waiting for your answer. When you are done, call thread_reply with token a, once."
	if d := agentmeta.Describe(routed, meta); len(d) != 1 || d[0].Facts.Form != threadtools.FormWhole {
		t.Fatalf("a routing line main never wrote read as %+v", d)
	}
}

// A row main wrote whose attribution names no thread is attributed to the
// thread its text names, opened only when its computer is this one.
func TestDescribeNamesMainsUnattributedSenderFromItsText(t *testing.T) {
	request := "Ship it.\n\n---\nAgent request from thread \"L\" (t-l).\nIt is not waiting for an answer. If it should hear something back, call thread_reply with token a, once: the reply reaches it as a message, and it sees only your reply text."
	for _, tc := range []struct {
		text, sendID string
		want         agentmeta.OriginThread
	}{
		{request, "thread-request:a", agentmeta.OriginThread{ThreadID: "t-l", Title: "L", Token: "a"}},
		{"Reply from \"W\" (t-w) [token k]:\nDone.", "thread-wake:k", agentmeta.OriginThread{ThreadID: "t-w", Title: "W", Token: "k"}},
		{"\"W\" (t-w, on desk) [token k] errored: boom", "thread-wake:k", agentmeta.OriginThread{Title: "W", Token: "k", ComputerName: "desk"}},
	} {
		described := agentmeta.Describe(tc.text, agentmeta.Meta{Origin: agentmeta.OriginAgentThread, SendID: tc.sendID})
		if len(described) != 1 || described[0].Thread == nil || *described[0].Thread != tc.want || described[0].Facts.Form == threadtools.FormWhole {
			t.Errorf("%q read as %+v, want it from %+v", tc.text, described, tc.want)
		}
	}
}

// A joined row whose recorded members do not describe its text is shown
// whole, every member's words in it: the first member's facts describe
// that member alone.
func TestDescribeShowsAJoinedRowOfUnknownMembersWhole(t *testing.T) {
	var quoting corpusCase
	for _, tc := range readCorpus(t) {
		if tc.Name == "two wakes joined, the first answer quoting a wake" {
			quoting = tc
		}
	}
	origin := quoting.Origins[0]
	for name, parts := range map[string][]agentmeta.JoinedPart{
		"no parts":            nil,
		"parts that miss":     {{Bytes: quoting.Members[0] - 1, Origin: agentmeta.OriginAgentThread, OriginThread: &origin}, {Bytes: quoting.Members[1] + 1, Origin: agentmeta.OriginAgentThread, OriginThread: &origin}},
		"parts past the text": {{Bytes: len(quoting.Text), Origin: agentmeta.OriginAgentThread, OriginThread: &origin}, {Bytes: 1}},
	} {
		meta := usermessage.Meta{Origin: agentmeta.OriginAgentThread, OriginThread: &origin, JoinedSendIDs: []string{"thread-wake:a", "thread-wake:b"}, JoinedParts: parts}.Agent()
		described := agentmeta.Describe(quoting.Text, meta)
		if len(described) != 1 || described[0].Facts.Form != threadtools.FormWhole || described[0].Body() != quoting.Text ||
			!strings.Contains(described[0].Body(), "B's answer, naïve") {
			t.Errorf("%s: %+v", name, described)
		}
	}
}

// Facts the text was changed behind the back of are not read: the message
// is its whole text, attributed, with nothing guessed.
func TestDescribeShowsAMessageWhoseTextChangedWhole(t *testing.T) {
	request, err := usermessage.Request(store.ThreadRequestSpawn, usermessage.Sender{ThreadID: "t-lead", Title: "Lead"},
		threadtools.Footer{Title: "Lead", ThreadID: "t-lead", Token: "k"}, "\n\n  Build   the docs  \nand more")
	if err != nil {
		t.Fatal(err)
	}
	attribution := request.Attribution()
	meta := agentmeta.Meta{Origin: attribution.Origin, OriginThread: attribution.Thread, SendID: "thread-request:k"}
	if d := agentmeta.Describe(request.Text(), meta); d[0].Body() != "Build   the docs  \nand more" || d[0].Facts.Headline != "Build the docs" {
		t.Fatalf("as written: %+v", d)
	}
	for _, edited := range []string{strings.TrimSpace(request.Text()), request.Text() + "\nmore", strings.Replace(request.Text(), "Build", "Built", 1)[:len(request.Text())-1]} {
		d := agentmeta.Describe(edited, meta)
		if len(d) != 1 || d[0].Facts.Form != threadtools.FormWhole || d[0].Body() != edited || d[0].Thread.ThreadID != "t-lead" {
			t.Errorf("%q: %+v", edited, d)
		}
	}
	// A report's facts that do not hold are not read either.
	report := built(t)(usermessage.Report(agentmeta.ReportLoop, usermessage.Reporter{Label: "l"}, "Loop fired.", "note: \"x\"", "x"))
	wake := report.Attribution().Wake
	if d := agentmeta.Describe(report.Text()+"!", agentmeta.Meta{Origin: agentmeta.OriginAgentWake, OriginWake: wake}); d[0].Facts.Form != threadtools.FormWhole {
		t.Fatalf("an edited report read as %+v", d[0].Facts)
	}
}
