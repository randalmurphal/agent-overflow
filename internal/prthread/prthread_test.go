package prthread

import (
	"strings"
	"testing"
	"unicode/utf8"

	gitops "agent-overflow/internal/git"
)

func TestBuildUserMessageFenceBeatsTripleBacktick(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 1}
	meta := gitops.PRMetadata{Title: "Demo"}
	diff := "diff --git a/README.md b/README.md\n" +
		"+ Example:\n" +
		"+ ```go\n" +
		"+ fmt.Println(\"hello\")\n" +
		"+ ```\n"

	msg := BuildUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	if len(fence) <= 3 {
		t.Fatalf("fence %q is too short to contain inner ``` runs", fence)
	}
	patch := extractPatchBlock(t, msg)
	if !strings.Contains(patch, "fmt.Println(\"hello\")") {
		t.Fatalf("patch content lost; got %q", patch)
	}
	if !strings.Contains(patch, "```go") {
		t.Fatalf("inner triple-backtick line not preserved; got %q", patch)
	}
}

func TestBuildUserMessageFenceOutlivesLongestRun(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 2}
	meta := gitops.PRMetadata{Title: "Huge fences"}
	diff := "+ " + strings.Repeat("`", 10) + "\n+ not closed yet\n"

	msg := BuildUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	if len(fence) < 11 {
		t.Fatalf("expected fence >= 11 backticks, got %d: %q", len(fence), fence)
	}
	patch := extractPatchBlock(t, msg)
	if !strings.Contains(patch, strings.Repeat("`", 10)) {
		t.Fatalf("10-backtick run dropped from patch; got %q", patch)
	}
	if !strings.Contains(patch, "not closed yet") {
		t.Fatalf("line after the backtick run lost; got %q", patch)
	}
}

func TestBuildUserMessageFenceFallsBackToThree(t *testing.T) {
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "tool", Number: 3}
	meta := gitops.PRMetadata{Title: "Plain diff"}
	diff := "diff --git a/foo.go b/foo.go\n+ println(\"hi\")\n"

	msg := BuildUserMessage(ref, meta, diff)
	fence := patchFenceFrom(t, msg)
	if fence != "```" {
		t.Fatalf("expected triple-backtick fence for backtick-free diff, got %q", fence)
	}
}

func TestBuildUserMessageGitLabHeaderUsesMRSigil(t *testing.T) {
	ref := gitops.PRReference{Forge: "gitlab", Namespace: "group", Repo: "tool", Number: 7}
	meta := gitops.PRMetadata{Title: "Demo MR"}

	msg := BuildUserMessage(ref, meta, "diff --git a/x b/x\n")
	if !strings.HasPrefix(msg, "# MR !7: Demo MR") {
		l := len(msg)
		if l > 60 {
			l = 60
		}
		t.Fatalf("gitlab header missing; got prefix %q", msg[:l])
	}
}

func TestFormatTitleGitHub(t *testing.T) {
	if got := FormatTitle("github", 12, " Add feature  "); got != "PR #12: Add feature" {
		t.Fatalf("FormatTitle = %q", got)
	}
}

func TestFormatTitleGitLab(t *testing.T) {
	if got := FormatTitle("gitlab", 7, "Fix it"); got != "MR !7: Fix it" {
		t.Fatalf("FormatTitle = %q", got)
	}
}

func TestTruncateTitleASCIIShort(t *testing.T) {
	title := "PR #1: tiny"
	if got := TruncateTitle(title); got != title {
		t.Fatalf("got %q, want %q (unchanged)", got, title)
	}
}

func TestTruncateTitleASCIIAtBoundary(t *testing.T) {
	title := strings.Repeat("a", MaxTitleRunes)
	if got := TruncateTitle(title); got != title {
		t.Fatalf("120-rune title mutated: got %q", got)
	}
}

func TestTruncateTitleASCIIOverflow(t *testing.T) {
	title := strings.Repeat("a", 200)
	got := TruncateTitle(title)
	if utf8.RuneCountInString(got) != MaxTitleRunes {
		t.Fatalf("rune count = %d, want %d", utf8.RuneCountInString(got), MaxTitleRunes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
}

func TestTruncateTitleMultibyteRunes(t *testing.T) {
	title := strings.Repeat("a", 200) + strings.Repeat("你", 20)
	got := TruncateTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != MaxTitleRunes {
		t.Fatalf("rune count = %d, want %d", utf8.RuneCountInString(got), MaxTitleRunes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
}

func TestTruncateTitleLeadingMultibyte(t *testing.T) {
	title := strings.Repeat("你", 150)
	got := TruncateTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != MaxTitleRunes {
		t.Fatalf("rune count = %d, want %d", utf8.RuneCountInString(got), MaxTitleRunes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	body := strings.TrimSuffix(got, "...")
	if utf8.RuneCountInString(body) != MaxTitleRunes-3 {
		t.Fatalf("body rune count = %d, want %d", utf8.RuneCountInString(body), MaxTitleRunes-3)
	}
	if strings.Contains(body, "a") {
		t.Fatalf("unexpected ASCII in body: %q", body)
	}
}

func TestTruncateTitleCombiningMarkPreservesValidity(t *testing.T) {
	// Use the NFD decomposed form: e + U+0301 combining acute = 2 runes.
	// 100 instances = 200 runes, well over the 120-rune cap. The combiner
	// can be separated from its base, but the output must still be valid
	// UTF-8 (the combining mark alone is a real rune). We don't promise
	// NFC/NFD integrity, just that we never emit invalid UTF-8.
	baseWithCombiner := "é"
	title := strings.Repeat(baseWithCombiner, 100)
	got := TruncateTitle(title)
	if !utf8.ValidString(got) {
		t.Fatalf("output not valid UTF-8: %q", got)
	}
	if utf8.RuneCountInString(got) != MaxTitleRunes {
		t.Fatalf("rune count = %d, want %d", utf8.RuneCountInString(got), MaxTitleRunes)
	}
}

func TestFenceForContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "```"},
		{"no backticks", "hello world", "```"},
		{"single backtick", "`x`", "```"},
		{"double backtick", "``x``", "```"},
		{"triple backtick", "```", "````"},
		{"four backticks", "````", "`````"},
		{"backtick runs split by content", "``` foo ```", "````"},
		{"longest run wins", "``\nhello\n````\nworld\n```", "`````"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FenceForContent(tt.content)
			if got != tt.want {
				t.Fatalf("FenceForContent(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestTruncateDiffNoopBelowBudget(t *testing.T) {
	in := "diff --git a/foo b/foo\n+ small\n"
	if got := TruncateDiff(in); got != in {
		t.Fatalf("expected no truncation, got %q", got)
	}
}

func TestTruncateDiffAppendsMarker(t *testing.T) {
	in := strings.Repeat("x", MaxInlinedDiffBytes+1024)
	got := TruncateDiff(in)
	if !strings.Contains(got, "diff truncated at") {
		t.Fatalf("expected truncation marker; got tail %q", got[len(got)-80:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", MaxInlinedDiffBytes)) {
		t.Fatal("expected first MaxInlinedDiffBytes preserved")
	}
}

// extractPatchBlock locates the "## Patch" section and returns the content
// between the opening and closing fence. Fails the test if the structure
// doesn't match so assertions stay readable.
func extractPatchBlock(t *testing.T, msg string) string {
	t.Helper()
	patchHeader := "## Patch\n\n"
	idx := strings.Index(msg, patchHeader)
	if idx < 0 {
		t.Fatalf("message missing Patch section: %q", msg)
	}
	rest := msg[idx+len(patchHeader):]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		t.Fatalf("message ends abruptly after Patch header: %q", msg)
	}
	fenceLine := rest[:newline]
	fence := strings.TrimSuffix(fenceLine, "diff")
	body := rest[newline+1:]
	closing := "\n" + fence + "\n"
	end := strings.LastIndex(body, closing)
	if end < 0 {
		t.Fatalf("no closing fence %q found in message %q", fence, msg)
	}
	return body[:end]
}

func patchFenceFrom(t *testing.T, msg string) string {
	t.Helper()
	patchHeader := "## Patch\n\n"
	idx := strings.Index(msg, patchHeader)
	if idx < 0 {
		t.Fatalf("message missing Patch section: %q", msg)
	}
	rest := msg[idx+len(patchHeader):]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		t.Fatalf("message ends abruptly after Patch header: %q", msg)
	}
	fenceLine := rest[:newline]
	return strings.TrimSuffix(fenceLine, "diff")
}
