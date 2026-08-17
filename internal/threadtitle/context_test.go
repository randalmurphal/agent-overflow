package threadtitle

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatThreadContextFitsEntirely(t *testing.T) {
	got := FormatThreadContext([]Message{
		{Role: "user", Text: "  fix the resume flake  "},
		{Role: "assistant", Text: "It is the lease clear."},
		{Role: "user", Text: "ship it"},
	}, false)
	want := "USER:\nfix the resume flake\n\nASSISTANT:\nIt is the lease clear.\n\nUSER:\nship it"
	if got != want {
		t.Fatalf("FormatThreadContext() =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatThreadContextSkipsUnrenderableMessages(t *testing.T) {
	got := FormatThreadContext([]Message{
		{Role: "system", Text: "you are a helpful assistant"},
		{Role: "user", Text: "   "},
		{Role: "assistant", Text: "answer"},
	}, false)
	if got != "ASSISTANT:\nanswer" {
		t.Fatalf("FormatThreadContext() = %q", got)
	}
}

func TestFormatThreadContextRendersAttachmentsWithoutText(t *testing.T) {
	got := FormatThreadContext([]Message{
		{Role: "user", Text: "  ", AttachmentNames: []string{"a.png", "b.png"}},
	}, false)
	if got != "USER:\n[Attachments: a.png, b.png]" {
		t.Fatalf("FormatThreadContext() = %q", got)
	}

	got = FormatThreadContext([]Message{
		{Role: "user", Text: "look", AttachmentNames: []string{"a.png"}},
	}, false)
	if got != "USER:\nlook\n[Attachments: a.png]" {
		t.Fatalf("FormatThreadContext() with text = %q", got)
	}
}

func TestFormatThreadContextEmptyThread(t *testing.T) {
	if got := FormatThreadContext(nil, false); got != "" {
		t.Fatalf("FormatThreadContext(nil) = %q, want empty", got)
	}
	// A marker with nothing behind it is not a context. Callers read ""
	// as "no subject to name" and skip the run, which is the right answer
	// for a thread whose every row rendered to nothing.
	if got := FormatThreadContext(nil, true); got != "" {
		t.Fatalf("FormatThreadContext(nil, dropped) = %q, want empty", got)
	}
	nothingRenderable := []Message{{Role: "user", Text: "  "}, {Role: "system", Text: "hi"}}
	if got := FormatThreadContext(nothingRenderable, true); got != "" {
		t.Fatalf("FormatThreadContext(unrenderable, dropped) = %q, want empty", got)
	}
}

// TestFormatThreadContextDropsOldestFirst is the retention contract:
// under budget pressure the NEWEST turns survive, with the thread's
// first user message pinned back on top.
func TestFormatThreadContextDropsOldestFirst(t *testing.T) {
	messages := []Message{
		{Role: "user", Text: "original ask about worktree pruning"},
		{Role: "assistant", Text: strings.Repeat("m", 4_000)},
		{Role: "assistant", Text: strings.Repeat("n", 4_000)},
		{Role: "user", Text: "latest word"},
	}
	got := FormatThreadContext(messages, false)

	if !strings.HasPrefix(got, "USER:\noriginal ask about worktree pruning\n\n"+earlierTruncatedMarker) {
		t.Fatalf("first user message not pinned: %q", got[:min(200, len(got))])
	}
	if !strings.HasSuffix(got, "USER:\nlatest word") {
		t.Fatalf("newest message dropped: %q", got[max(0, len(got)-100):])
	}
	if strings.Contains(got, strings.Repeat("m", 4_000)) {
		t.Fatal("oldest assistant message retained in full despite truncation")
	}
	if !strings.Contains(got, strings.Repeat("n", 100)) {
		t.Fatal("newest assistant message dropped before the oldest one")
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestFormatThreadContextDoesNotPinARetainedFirstUserMessage is the
// duplication regression: the first user message sits comfortably inside
// the retained window while an older assistant message overruns the
// budget. Pinning it back on top would render the same ask twice.
func TestFormatThreadContextDoesNotPinARetainedFirstUserMessage(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Text: strings.Repeat("z", 9_000)},
		{Role: "user", Text: "the only ask"},
		{Role: "assistant", Text: "a short answer"},
	}
	got := FormatThreadContext(messages, false)

	if !strings.HasPrefix(got, earlierTruncatedMarker) {
		t.Fatalf("truncation not marked: %q", got[:min(120, len(got))])
	}
	if n := strings.Count(got, "the only ask"); n != 1 {
		t.Fatalf("first user message rendered %d times, want 1:\n%q", n, got[:min(400, len(got))])
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestFormatThreadContextCapsPinnedFirstUserMessage covers a thread
// whose opening message is itself enormous: it is pinned, capped, and
// marked, and the recent window still gets a share of the budget.
func TestFormatThreadContextCapsPinnedFirstUserMessage(t *testing.T) {
	messages := []Message{
		{Role: "user", Text: strings.Repeat("o", 9_000)},
		{Role: "assistant", Text: strings.Repeat("p", 6_000)},
		{Role: "user", Text: "later ask"},
	}
	got := FormatThreadContext(messages, false)

	pinned, rest, found := strings.Cut(got, "\n\n"+earlierTruncatedMarker)
	if !found {
		t.Fatalf("missing pinned/recent split: %q", got[:min(200, len(got))])
	}
	if !strings.HasSuffix(pinned, firstUserTruncatedMarker) {
		t.Fatalf("pinned section missing truncation marker: %q", pinned[max(0, len(pinned)-80):])
	}
	if len(pinned) > maxFirstUserChars {
		t.Fatalf("pinned length = %d, want <= %d", len(pinned), maxFirstUserChars)
	}
	if !strings.HasSuffix(rest, "USER:\nlater ask") {
		t.Fatalf("recent window lost the newest message: %q", rest[max(0, len(rest)-80):])
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestFormatThreadContextWithoutUserMessageMarksTruncation covers an
// assistant-only thread: nothing to pin, so the marker alone reports
// that earlier content was dropped — and the marker rides INSIDE the
// budget rather than on top of it.
func TestFormatThreadContextWithoutUserMessageMarksTruncation(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Text: strings.Repeat("q", 5_000)},
		{Role: "assistant", Text: strings.Repeat("r", 5_000)},
	}
	got := FormatThreadContext(messages, false)

	if !strings.HasPrefix(got, earlierTruncatedMarker) {
		t.Fatalf("missing truncation marker: %q", got[:min(120, len(got))])
	}
	if strings.Contains(got, "USER:") {
		t.Fatalf("no user message exists to pin: %q", got[:min(200, len(got))])
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestFormatThreadContextMarksStoreDroppedRows: the text fits the
// character budget whole, but the STORE's row window already excluded
// rows. Rendering it as a seamless transcript would tell the model
// nothing was dropped, which the regeneration prompt acts on.
func TestFormatThreadContextMarksStoreDroppedRows(t *testing.T) {
	messages := []Message{
		{Role: "user", Text: "middle ask"},
		{Role: "assistant", Text: "middle answer"},
		{Role: "user", Text: "latest ask"},
	}
	got := FormatThreadContext(messages, true)

	if !strings.HasPrefix(got, earlierTruncatedMarker) {
		t.Fatalf("dropped rows not marked: %q", got)
	}
	if !strings.HasSuffix(got, "USER:\nlatest ask") {
		t.Fatalf("newest message lost: %q", got)
	}
	// The window's own first user message was fully retained, so it must
	// not also be pinned on top.
	if n := strings.Count(got, "middle ask"); n != 1 {
		t.Fatalf("first user message rendered %d times, want 1:\n%q", n, got)
	}
	if unmarked := FormatThreadContext(messages, false); strings.Contains(unmarked, earlierTruncatedMarker) {
		t.Fatalf("rowsDropped=false must not mark a thread that fits: %q", unmarked)
	}
}

// TestFormatThreadContextTailKeepsRoleHeader: the section that overruns
// the budget contributes a tail, and that tail keeps its `ROLE:\n`
// header — the regeneration prompt's step 1 is "Read the USER messages
// first", which an unlabeled blob defeats.
func TestFormatThreadContextTailKeepsRoleHeader(t *testing.T) {
	messages := []Message{
		{Role: "user", Text: "opening ask " + strings.Repeat("w", 9_000) + " closing words"},
		{Role: "assistant", Text: "short answer"},
	}
	got := FormatThreadContext(messages, false)

	rest, found := strings.CutPrefix(got, earlierTruncatedMarker)
	if !found {
		// A pinned first-user message is the other legal shape; either
		// way the overrun tail below must be labeled.
		_, rest, found = strings.Cut(got, "\n\n"+earlierTruncatedMarker)
		if !found {
			t.Fatalf("missing truncation marker: %q", got[:min(200, len(got))])
		}
	}
	if !strings.HasPrefix(rest, "USER:\n") {
		t.Fatalf("overrun tail lost its role header: %q", rest[:min(120, len(rest))])
	}
	if !strings.Contains(rest, "closing words") {
		t.Fatal("overrun tail did not keep the section's most recent text")
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestFormatThreadContextPinsWhenMarkerBudgetDropsTheFirstUserMessage
// covers the band where the full-budget walk retains the first user
// message but the marker-budget re-collect cannot: the transcript fits
// maxContextChars yet not maxContextChars-len(marker). The pin decision
// must follow the walk whose output ships, or the original ask is
// dropped to make room for a truncation marker.
func TestFormatThreadContextPinsWhenMarkerBudgetDropsTheFirstUserMessage(t *testing.T) {
	ask := "the original ask " + strings.Repeat("a", 83) // 100 bytes
	// Sections render as "USER:\n"+ask (106) + "\n\n" + "ASSISTANT:\n"+reply.
	// Total 7_990: inside the 8_000 budget, outside 8_000-29.
	reply := strings.Repeat("b", 7_990-19-len(ask))
	messages := []Message{
		{Role: "user", Text: ask},
		{Role: "assistant", Text: reply},
	}

	got := FormatThreadContext(messages, true)

	if n := strings.Count(got, "the original ask"); n != 1 {
		t.Fatalf("first user message rendered %d times, want 1 (pinned):\n%q", n, got[:min(200, len(got))])
	}
	if !strings.HasPrefix(got, "USER:\nthe original ask") {
		t.Fatalf("first user message not pinned on top: %q", got[:min(120, len(got))])
	}
	if !strings.Contains(got, earlierTruncatedMarker) {
		t.Fatal("truncation not marked")
	}
	if len(got) > maxContextChars {
		t.Fatalf("context length = %d, want <= %d", len(got), maxContextChars)
	}
}

// TestCollectRecentContextDropsASectionItsHeaderCannotFit: when the
// remaining room is no bigger than the section's `ROLE:\n` header, the
// section contributes nothing at all — a bare label with no text is
// worse than an omission.
func TestCollectRecentContextDropsASectionItsHeaderCannotFit(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Text: strings.Repeat("x", 500)},
		{Role: "user", Text: "hi"},
	}
	// "USER:\nhi" (8) + separator (2) leaves exactly len("ASSISTANT:\n")
	// (11) — room for the header and nothing else.
	text, truncated, _ := collectRecentContext(messages, 8+2+11)

	if text != "USER:\nhi" {
		t.Fatalf("collected %q, want the newest section alone", text)
	}
	if !truncated {
		t.Fatal("dropped section not reported as truncation")
	}
}

// TestCollectRecentContextDropsASectionWhoseTailCutsToNothing: room can
// be positive yet smaller than one rune of the section's text, in which
// case TailRunes yields "" and the section must still contribute
// nothing rather than a bare `ROLE:` label.
func TestCollectRecentContextDropsASectionWhoseTailCutsToNothing(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Text: strings.Repeat("\U0001D11E", 200)}, // 4 bytes per rune
		{Role: "user", Text: "hi"},
	}
	// Room after the header is 2 bytes — inside the final 4-byte rune.
	text, truncated, _ := collectRecentContext(messages, 8+2+11+2)

	if text != "USER:\nhi" {
		t.Fatalf("collected %q, want the newest section alone", text)
	}
	if strings.Contains(text, "ASSISTANT") {
		t.Fatal("bare role label leaked into the collected context")
	}
	if !truncated {
		t.Fatal("dropped section not reported as truncation")
	}
}

// TestFormatThreadContextKeepsValidUTF8AtCuts is the property the byte
// budgets must not cost us: both cut sites (the recent-window tail and
// the pinned-message prefix) land on rune boundaries.
func TestFormatThreadContextKeepsValidUTF8AtCuts(t *testing.T) {
	multibyte := strings.Repeat("é", 5_000) // two bytes per rune
	messages := []Message{
		{Role: "user", Text: multibyte},
		{Role: "assistant", Text: multibyte},
		{Role: "user", Text: "tail"},
	}
	got := FormatThreadContext(messages, false)

	if !utf8.ValidString(got) {
		t.Fatal("formatted context is not valid UTF-8 — a cut landed mid-rune")
	}
	if !strings.Contains(got, firstUserTruncatedMarker) {
		t.Fatalf("expected the pinned message to be capped: %q", got[:min(120, len(got))])
	}
}
