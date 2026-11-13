package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
)

// The rewrite keeps the file's line endings and its trailing-newline
// state and replaces exactly the anchored lines.
func TestApplySuggestionToFile(t *testing.T) {
	t.Parallel()
	bom := "\xef\xbb\xbf"
	for _, tc := range []struct {
		name       string
		file       string
		start, end int
		lines      []string
		want       string
	}{
		{"single line in the middle", "a\nb\nc\n", 2, 2, []string{"B"}, "a\nB\nc\n"},
		{"first line", "a\nb\nc\n", 1, 1, []string{"X"}, "X\nb\nc\n"},
		{"range at the start", "a\nb\nc\n", 1, 2, []string{"X"}, "X\nc\n"},
		{"range at the end", "a\nb\nc\n", 2, 3, []string{"Y", "Z", "W"}, "a\nY\nZ\nW\n"},
		{"range in the middle", "a\nb\nc\nd\n", 2, 3, []string{"M"}, "a\nM\nd\n"},
		{"whole file", "a\nb\n", 1, 2, []string{"z"}, "z\n"},
		{"one-line file", "a\n", 1, 1, []string{"b", "c"}, "b\nc\n"},
		{"no trailing newline, last line replaced", "a\nb", 2, 2, []string{"c", "d"}, "a\nc\nd"},
		{"no trailing newline, last line deleted", "a\nb", 2, 2, []string{}, "a\n"},
		{"no trailing newline, last line untouched", "a\nb", 1, 1, []string{"z"}, "z\nb"},
		{"no trailing newline, one line, no ending anywhere", "a", 1, 1, []string{"b", "c"}, "b\nc"},
		{"no trailing newline, CRLF before it", "a\r\nb", 2, 2, []string{"c", "d"}, "a\r\nc\r\nd"},
		{"CRLF file", "a\r\nb\r\nc\r\n", 2, 2, []string{"B", "B2"}, "a\r\nB\r\nB2\r\nc\r\n"},
		{"a CRLF line in an LF file", "a\nb\r\nc\n", 2, 2, []string{"x", "y"}, "a\nx\r\ny\r\nc\n"},
		{"an LF line in a CRLF file", "a\r\nb\nc\r\n", 2, 2, []string{"x", "y"}, "a\r\nx\ny\nc\r\n"},
		{"mixed endings outside the range kept", "a\r\nb\nc\r\nd", 3, 3, []string{"C"}, "a\r\nb\nC\r\nd"},
		{"a CR-only line kept", "\r\na\n\r\n", 2, 2, []string{"b"}, "\r\nb\n\r\n"},
		{"a lone CR inside a line is content", "a\rb\nc\n", 2, 2, []string{"C"}, "a\rb\nC\n"},
		{"deletion", "a\nb\nc\n", 2, 2, []string{}, "a\nc\n"},
		{"deleting every line", "a\n", 1, 1, []string{}, ""},
		{"empty replacement of a range", "a\nb\nc\nd\n", 2, 3, []string{}, "a\nd\n"},
		{"a blank replacement line", "a\nb\n", 1, 1, []string{""}, "\nb\n"},
		{"BOM kept when line 1 is replaced", bom + "a\nb\n", 1, 1, []string{"z"}, bom + "z\nb\n"},
		{"BOM untouched below line 1", bom + "a\nb\n", 2, 2, []string{"z"}, bom + "a\nz\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := applySuggestionToFile([]byte(tc.file), tc.start, tc.end, tc.lines)
			if err != nil || string(got) != tc.want {
				t.Fatalf("= %q, %v; want %q", got, err, tc.want)
			}
			// The bytes before line start and after line end's ending are
			// the input's, counted independently of the code under test.
			lines := strings.SplitAfter(tc.file, "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			prefix := strings.Join(lines[:tc.start-1], "")
			if tc.start == 1 && strings.HasPrefix(tc.file, bom) {
				prefix = bom
			}
			suffix := strings.Join(lines[tc.end:], "")
			if !strings.HasPrefix(string(got), prefix) || !strings.HasSuffix(string(got), suffix) {
				t.Fatalf("%q does not keep prefix %q and suffix %q", got, prefix, suffix)
			}
		})
	}
	for _, tc := range []struct {
		name       string
		file       string
		start, end int
	}{
		{"binary", "a\x00b\n", 1, 1},
		{"not UTF-8", "a\xffb\n", 1, 1},
		{"past the end", "a\nb\n", 2, 3},
		{"empty file", "", 1, 1},
		{"backwards", "a\nb\n", 2, 1},
	} {
		if _, err := applySuggestionToFile([]byte(tc.file), tc.start, tc.end, []string{"x"}); !errors.As(err, new(*SuggestionNotAppliedError)) {
			t.Fatalf("%s: err = %v, want a refusal", tc.name, err)
		}
	}
}

// GitHub's suggestions come from the body on the thread's new-side
// anchor; Apply is offered only on an open PR the viewer may apply to,
// on a current, unresolved thread on new lines. A widened GitLab header
// is code on GitHub.
func TestGitHubMarkSuggestions(t *testing.T) {
	t.Parallel()
	line, start := 7, 5
	body := "```suggestion\nnew\n```\n```suggestion:-1+0\nnot on GitHub\n```\n```suggestion\n```"
	thread := func(mod func(*ReviewThread)) ReviewThread {
		th := ReviewThread{Path: "a.go", Side: "right", Line: &line, StartLine: &start, StartSide: "right", IsResolvable: true, Comments: []ReviewComment{{Body: body}}}
		mod(&th)
		return th
	}
	open := &githubPullRaw{State: "OPEN", ViewerCanApplySuggestion: true}
	for _, tc := range []struct {
		name      string
		thread    ReviewThread
		pull      *githubPullRaw
		appliable bool
		lines     [2]int
	}{
		{"current range", thread(func(*ReviewThread) {}), open, true, [2]int{5, 7}},
		{"single line", thread(func(th *ReviewThread) { th.StartLine, th.StartSide = nil, "" }), open, true, [2]int{7, 7}},
		{"closed PR", thread(func(*ReviewThread) {}), &githubPullRaw{State: "CLOSED", ViewerCanApplySuggestion: true}, false, [2]int{5, 7}},
		{"viewer may not apply", thread(func(*ReviewThread) {}), &githubPullRaw{State: "OPEN"}, false, [2]int{5, 7}},
		{"outdated", thread(func(th *ReviewThread) { th.IsOutdated, th.Line, th.StartLine = true, nil, nil }), open, false, [2]int{}},
		{"resolved", thread(func(th *ReviewThread) { th.IsResolved = true }), open, false, [2]int{5, 7}},
		{"old side", thread(func(th *ReviewThread) { th.Side, th.StartSide = "left", "left" }), open, false, [2]int{}},
		{"range from the old side", thread(func(th *ReviewThread) { th.StartSide = "left" }), open, false, [2]int{}},
		{"file", thread(func(th *ReviewThread) { th.Side, th.Line, th.StartLine = "file", nil, nil }), open, false, [2]int{}},
	} {
		threads := []ReviewThread{tc.thread}
		githubMarkSuggestions(threads, tc.pull)
		got := threads[0].Comments[0].Suggestions
		want := []CommentSuggestion{
			{Index: 0, StartLine: tc.lines[0], EndLine: tc.lines[1], To: []string{"new"}, Appliable: tc.appliable},
			{Index: 1, StartLine: tc.lines[0], EndLine: tc.lines[1], To: []string{}, Appliable: tc.appliable},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: suggestions = %+v, want %+v", tc.name, got, want)
		}
	}
	plain := []ReviewThread{{Path: "a.go", Side: "right", Line: &line, Comments: []ReviewComment{{Body: "no fence"}}}}
	githubMarkSuggestions(plain, open)
	if plain[0].Comments[0].Suggestions != nil {
		t.Fatalf("a comment without a suggestion carries %+v", plain[0].Comments[0].Suggestions)
	}
}

func TestGitHubReactionsMapTheSharedVocabulary(t *testing.T) {
	t.Parallel()
	group := func(content string, n int, viewer bool) githubReactionGroupRaw {
		g := githubReactionGroupRaw{Content: content, ViewerHasReacted: viewer}
		g.Reactors.TotalCount = n
		return g
	}
	got := githubReactions([]githubReactionGroupRaw{
		group("THUMBS_UP", 2, true), group("THUMBS_DOWN", 0, false), group("LAUGH", 1, false), group("HOORAY", 1, false),
		group("CONFUSED", 1, false), group("HEART", 3, true), group("ROCKET", 1, false), group("EYES", 1, false), group("FUTURE", 4, false),
	})
	want := []CommentReaction{
		{Name: "thumbsup", Count: 2, ViewerReacted: true}, {Name: "laughing", Count: 1}, {Name: "tada", Count: 1},
		{Name: "confused", Count: 1}, {Name: "heart", Count: 3, ViewerReacted: true}, {Name: "rocket", Count: 1}, {Name: "eyes", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %+v", got)
	}
	if githubReactions(nil) != nil {
		t.Fatal("no groups is no reactions")
	}
}

// GitLab's suggestions are the forge's own, Apply withheld below the
// Developer role its API requires.
func TestGitLabSuggestionsAreTheForgesOwn(t *testing.T) {
	t.Parallel()
	got := gitlabSuggestions([]gitlabSuggestionRaw{
		{ID: 11, FromLine: 4, ToLine: 5, Appliable: true, FromContent: "a\nb\n", ToContent: "c\n"},
		{ID: 12, FromLine: 9, ToLine: 9, Applied: true, FromContent: "x\r\n", ToContent: ""},
	})
	want := []CommentSuggestion{
		{Index: 0, ID: "11", StartLine: 4, EndLine: 5, From: []string{"a", "b"}, FromKnown: true, To: []string{"c"}, Appliable: true},
		{Index: 1, ID: "12", StartLine: 9, EndLine: 9, From: []string{"x"}, FromKnown: true, To: []string{}, Applied: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions = %+v", got)
	}
	threads := []ReviewThread{{Comments: []ReviewComment{{Suggestions: got}}}}
	gateGitLabSuggestions(threads, gitlabDeveloperAccess)
	if !threads[0].Comments[0].Suggestions[0].Appliable {
		t.Fatal("a developer lost Apply")
	}
	gateGitLabSuggestions(threads, 20)
	if threads[0].Comments[0].Suggestions[0].Appliable {
		t.Fatal("a reporter is offered Apply")
	}
}

func TestSuggestionRequestsAndReactionNamesAreChecked(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	target := CommentTarget{ThreadID: "t", CommentID: "c"}
	for _, tc := range []struct {
		forge string
		req   SuggestionApply
		ok    bool
	}{
		{"github", SuggestionApply{Target: target, HeadSHA: head}, true},
		{"github", SuggestionApply{Target: target, HeadSHA: "abc"}, false},
		{"github", SuggestionApply{Target: CommentTarget{ThreadID: "t"}, HeadSHA: head}, false},
		{"github", SuggestionApply{Target: target, HeadSHA: head, Index: -1}, false},
		{"gitlab", SuggestionApply{Target: target, HeadSHA: head, SuggestionID: "12"}, true},
		{"gitlab", SuggestionApply{Target: target, HeadSHA: head}, false},
		{"gitlab", SuggestionApply{Target: target, HeadSHA: head, SuggestionID: "x"}, false},
	} {
		if err := tc.req.Validate(tc.forge); (err == nil) != tc.ok {
			t.Fatalf("%s %+v: %v", tc.forge, tc.req, err)
		}
	}
	for _, tc := range []struct {
		forge, name string
		ok          bool
	}{
		{"github", "thumbsup", true}, {"github", "eyes", true}, {"github", "100", false}, {"github", "THUMBS_UP", false},
		{"gitlab", "thumbsup", true}, {"gitlab", "100", true}, {"gitlab", "slight_smile", true}, {"gitlab", ":x:", false}, {"gitlab", "", false},
	} {
		if err := ValidateReactionName(tc.forge, tc.name); (err == nil) != tc.ok {
			t.Fatalf("%s %q: %v", tc.forge, tc.name, err)
		}
	}
}

// An apply's outcomes read as one sentence each: a refusal before the
// write, a commit that may have landed, and a commit whose thread was
// not resolved, which never advises a retry.
func TestSuggestionOutcomesAreExplained(t *testing.T) {
	t.Parallel()
	ref := githubTestRef(5)
	explain := func(err error) *ForgeActionError {
		var out *ForgeActionError
		if !errors.As(ExplainForgeError(ref, ActionApplySuggestion, err), &out) {
			t.Fatalf("%v is not explained", err)
		}
		return out
	}
	if e := explain(&SuggestionNotAppliedError{Reason: "the file is not text"}); e.Kind != ForgeErrorRefused || e.Message != "The suggestion was not applied: the file is not text." {
		t.Fatalf("refusal = %+v", e)
	}
	if e := explain(&UnconfirmedWriteError{Err: errors.New("reset")}); e.Kind != ForgeErrorUnconfirmed || e.Message != "GitHub did not confirm the request to apply a suggestion on o/r#5, so it may have been applied. The pane shows what GitHub did." {
		t.Fatalf("unconfirmed = %+v", e)
	}
	moved := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "UNPROCESSABLE", Message: `Expected branch to point to "abc" but it did not. Pull and try again.`}}}
	if e := explain(moved); e.Kind != ForgeErrorStaleHead {
		t.Fatalf("moved head = %+v", e)
	}
	refused := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "FORBIDDEN", Message: "Resource not accessible"}}}
	partial := explain(&PartialWriteError{Done: "committed the suggestion to the head branch", Next: "resolving its thread", Err: refused})
	if partial.Message != "Committed the suggestion to the head branch of o/r#5; resolving its thread was refused: GitHub did not apply a suggestion on o/r#5: Resource not accessible" {
		t.Fatalf("partial refused = %q", partial.Message)
	}
	lost := explain(&PartialWriteError{Done: "committed the suggestion to the head branch", Next: "resolving its thread", Err: &forgeapi.TransientError{Err: errors.New("reset")}})
	if lost.Kind != ForgeErrorUnconfirmed || lost.Message != "Committed the suggestion to the head branch of o/r#5; resolving its thread may have applied. The pane shows what GitHub did." {
		t.Fatalf("partial unconfirmed = %+v", lost)
	}
	for _, e := range []*ForgeActionError{partial, lost} {
		if strings.Contains(strings.ToLower(e.Message), "try again") {
			t.Fatalf("a partial apply advises a retry: %q", e.Message)
		}
	}
}
