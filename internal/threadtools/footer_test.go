package threadtools

import (
	"strings"
	"testing"
	"time"
)

// TestWaitingFooter: the block a spawn's first message or a send carries
// when the sender waits or asked to be notified. It opens with the "Agent
// request" marker the Answering paragraph tells the receiver to look for.
func TestWaitingFooter(t *testing.T) {
	got := Footer{
		Title: "Build the parser", ThreadID: "7f3a", Computer: "Studio", Token: "tok-1",
		AnswerRequested: true, SenderReachable: true,
		UserMessage: "make the parser handle comments",
	}.String()
	want := strings.Join([]string{
		"---",
		`Agent request from thread "Build the parser" (7f3a, on Studio).`,
		"It is waiting for your answer. When you are done, call thread_reply with token tok-1, once. The sender sees only your reply text, so make it self-contained. To read the sender's thread, use thread_show with its id.",
		`The user's latest message in that thread: "make the parser handle comments"`,
	}, "\n")
	if got != want {
		t.Fatalf("footer:\n%s\nwant:\n%s", got, want)
	}
}

// TestNotWaitingFooter: the token and thread_reply are given either way,
// since a reply always reaches the request record.
func TestNotWaitingFooter(t *testing.T) {
	got := Footer{Title: "Nightly", ThreadID: "91bb", Token: "tok-2", SenderReachable: true}.String()
	want := strings.Join([]string{
		"---",
		`Agent request from thread "Nightly" (91bb).`,
		"It is not waiting. No answer notification was requested. To reply anyway, call thread_reply with token tok-2, once. To answer it, use thread_send with its id.",
	}, "\n")
	if got != want {
		t.Fatalf("footer:\n%s\nwant:\n%s", got, want)
	}
}

// TestFooterOmitsWhatDoesNotApply: no computer when the sender is on this
// one, no thread_show or thread_send offer when the receiver holds no
// credential for the sender, and no quoted line when there is no user
// message to quote.
func TestFooterOmitsWhatDoesNotApply(t *testing.T) {
	waiting := Footer{Title: "Local", ThreadID: "abc", Token: "t", AnswerRequested: true}.String()
	if strings.Contains(waiting, ", on ") {
		t.Errorf("footer names a computer when the sender is local: %s", waiting)
	}
	if strings.Contains(waiting, "thread_show") {
		t.Errorf("footer offers thread_show to a receiver that cannot reach the sender: %s", waiting)
	}
	if strings.Contains(waiting, "The user's latest message") {
		t.Errorf("footer quotes a user message that does not exist: %s", waiting)
	}
	quiet := Footer{Title: "Local", ThreadID: "abc", Token: "t"}.String()
	if strings.Contains(quiet, "thread_send") {
		t.Errorf("footer offers thread_send to a receiver that cannot reach the sender: %s", quiet)
	}
	if !strings.Contains(quiet, "token t") {
		t.Errorf("a footer without a wait still has to carry the token: %s", quiet)
	}
}

// TestFooterCapsTheQuotedMessage folds the quote onto one line and caps it
// so a long user message cannot turn the footer into a second message.
func TestFooterCapsTheQuotedMessage(t *testing.T) {
	long := strings.Repeat("a", UserMessageQuoteRunes+50)
	got := Footer{Title: "T", ThreadID: "i", Token: "t", UserMessage: "first\nsecond\n\n" + long}.String()
	quoted := got[strings.Index(got, "The user's latest message"):]
	if strings.Count(quoted, "\n") != 0 {
		t.Errorf("the quoted line is not one line: %q", quoted)
	}
	if !strings.Contains(quoted, "first second ") || !strings.HasSuffix(quoted, "…\"") {
		t.Errorf("quote was not folded and capped: %q", quoted)
	}
}

// TestWakeTemplates pins each status line. on <computer> and answered
// <age> ago appear only when they apply, and every line carries the token
// the whole answer is reachable by.
func TestWakeTemplates(t *testing.T) {
	cases := []struct {
		name string
		wake Wake
		want string
	}{
		{
			name: "reply from another computer with an age",
			wake: Wake{Kind: WakeReply, Title: "Parser", ThreadID: "7f3a", Computer: "Studio", Token: "tok", Age: 3 * time.Hour, Text: "done"},
			want: "Reply from \"Parser\" (7f3a, on Studio, answered 3h ago) [token tok]:\ndone",
		},
		{
			name: "fresh local reply states no computer and no age",
			wake: Wake{Kind: WakeReply, Title: "Parser", ThreadID: "7f3a", Token: "tok", Age: 2 * time.Second, Text: "done"},
			want: "Reply from \"Parser\" (7f3a) [token tok]:\ndone",
		},
		{
			name: "finished without replying",
			wake: Wake{Kind: WakeFinished, Title: "Parser", ThreadID: "7f3a", Token: "tok", Text: "I started the tests"},
			want: "\"Parser\" (7f3a) [token tok] finished its turn without calling thread_reply. Its final message:\nI started the tests",
		},
		{
			name: "errored",
			wake: Wake{Kind: WakeErrored, Title: "Parser", ThreadID: "7f3a", Token: "tok", Text: "the provider refused"},
			want: "\"Parser\" (7f3a) [token tok] errored: the provider refused",
		},
		{
			name: "cancelled",
			wake: Wake{Kind: WakeCancelled, Title: "Parser", ThreadID: "7f3a", Token: "tok"},
			want: "\"Parser\" (7f3a) [token tok] was cancelled.",
		},
		{
			name: "interrupted by a restart of another computer",
			wake: Wake{Kind: WakeInterrupted, Title: "Parser", ThreadID: "7f3a", Computer: "Studio", Token: "tok"},
			want: "\"Parser\" (7f3a, on Studio) [token tok] was interrupted by a restart of Studio.",
		},
		{
			name: "interrupted locally names this computer",
			wake: Wake{Kind: WakeInterrupted, Title: "Parser", ThreadID: "7f3a", Token: "tok"},
			want: "\"Parser\" (7f3a) [token tok] was interrupted by a restart of this computer.",
		},
		{
			name: "expired",
			wake: Wake{Kind: WakeExpired, Title: "Parser", ThreadID: "7f3a", Computer: "Studio", Token: "tok"},
			want: "\"Parser\" (7f3a, on Studio) [token tok] did not answer within a day.",
		},
		{
			name: "reminder",
			wake: Wake{Kind: WakeReminder, Token: "tok", Text: "check the deploy"},
			want: "Reminder you set [token tok]:\ncheck the deploy",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.wake.String(); got != testCase.want {
				t.Fatalf("wake:\n%s\nwant:\n%s", got, testCase.want)
			}
		})
	}
}

// TestWakeBodyPreviewPointer: a body over the preview budget ends with the
// pointer to the whole answer, and one inside it is untouched.
func TestWakeBodyPreviewPointer(t *testing.T) {
	short := strings.Repeat("x", WakePreviewBytes)
	if got := WakeBody(short, "tok"); got != short {
		t.Fatalf("a body inside the budget was changed")
	}
	long := strings.Repeat("x", WakePreviewBytes+1)
	got := WakeBody(long, "tok")
	if !strings.HasSuffix(got, "\n[preview; thread_status tok for the whole answer]") {
		t.Fatalf("no preview pointer: %q", got[len(got)-80:])
	}
	if len(got) != WakePreviewBytes+1+len(WakePreviewPointer("tok")) {
		t.Fatalf("preview is %d bytes of body, want %d", len(got)-1-len(WakePreviewPointer("tok")), WakePreviewBytes)
	}
}

// TestWakeBodyClipsOnACharacterBoundary keeps a multi-byte character from
// being cut in half by the preview budget.
func TestWakeBodyClipsOnACharacterBoundary(t *testing.T) {
	body := strings.Repeat("x", WakePreviewBytes-1) + "é" + "tail"
	got := WakeBody(body, "tok")
	preview := strings.TrimSuffix(got, "\n"+WakePreviewPointer("tok"))
	if strings.HasSuffix(preview, "\xc3") {
		t.Fatalf("the preview split a character: %q", preview[len(preview)-4:])
	}
	if len(preview) != WakePreviewBytes-1 {
		t.Fatalf("preview is %d bytes, want %d", len(preview), WakePreviewBytes-1)
	}
}

func TestAge(t *testing.T) {
	for _, testCase := range []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	} {
		if got := Age(testCase.in); got != testCase.want {
			t.Errorf("Age(%v) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}
