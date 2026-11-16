package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFindLooseFoldsAndDropsASCIIPunctuation(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Hello, World!", "helloworld"},
		{"**foo**bar", "foobar"},
		{"a\n\tb  c", "abc"},
		{"École — Straße", "école—straße"},
		{"x\x00y", "xy"},
		{"100%_done", "100done"},
		{"", ""},
	} {
		if got := findLoose(tc.in); got != tc.want {
			t.Errorf("findLoose(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The shown variant drops what renders no letters, outside code only, and
// is unsure of text whose markup it cannot read as the lexer does.
func TestFindShownMarkdownErasesOnlyWhatRendersNoLetters(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[docs](https://example.com/x)suffix", "docssuffix"},
		{"[ref][label]x", "[ref][label]x"},
		{"see ![alt](a.png)now", "see now"},
		{"<b>foo</b>bar", "foobar"},
		{"a <!-- note --> b", "a  b"},
		{"AT&amp;T <b>x</b>", "AT&amp;T x"},
		{"`<b>` and <i>y</i>z", "`<b>` and yz"},
		{"``[a](b)`` [c](d)e", "``[a](b)`` ce"},
		{"```\n[a](b)<b>\n```\n[c](d)e", "```\n[a](b)<b>\n```\nce"},
		{"plain text", "plain text"},
		{"<https://example.com> <user@example.com>", "<https://example.com> <user@example.com>"},
		{`a \[b](literal) c [d](https://example.com) e`, `a \[b](literal) c d e`},
		{"x](y) [a](https://e.io/w_(z)) b", "x](y) a b"},
		{"See `foo()` in [file.go](src/file.go) and <placeholder> here, $5 or $6.", "See `foo()` in file.go and  here, $5 or $6."},
		{"Visit https://example.com/a_(b) then [c](d).", "Visit https://example.com/a_(b) then c."},
		{"| a | [b](c) |\n| --- | --- |\n| `d` | e |", "| a | b |\n| --- | --- |\n| `d` | e |"},
		{"1. Run:\n   ```sh\n   x < y\n   ```\n2. See [z](w)", "1. Run:\n   ```sh\n   x < y\n   ```\n2. See z"},
		// A fence that may sit in a container is not read: its lines scan.
		{"- a\n  ```\n  x\n\n[b](c) d\n\n```\ny\n```", "- a\n  ```\n  x\n\nb d\n\n```\ny\n```"},
		{"```markdown\n[a](b) c\n```", "```markdown\na c\n```"},
	} {
		got, sure := findShownMarkdown(tc.in)
		if !sure || got != tc.want {
			t.Errorf("findShownMarkdown(%q) = %q, %v; want %q", tc.in, got, sure, tc.want)
		}
	}
	for _, in := range []string{
		"[a [b](c) d](e) f",
		"[t](https://e.io \"title\") and [u](v)",
		"[a](<b c>) d",
		"x `a [b](c) y",
		"[x `y] [a](b) `c`",
		"<!-- open [a](b)",
		"$`$ [a](b) `c`",
		"~~a `b~~ [c](d) `e`",
		"https://e.io/`a [b](c) d`",
		"[l]: https://e.io\n[t][l] and [a](b)",
		"<Note>`a</Note> [x](y) `b`",
		"| `a | [b](c) | d` |",
		"<a x=\n\"1\"> [b](c)",
		"<a x=\n> \"1\"> [b](c)",
		"- ```\n  x\n  ```\n\n[a](b) c",
	} {
		if got, sure := findShownMarkdown(in); sure {
			t.Errorf("findShownMarkdown(%q) = %q, sure", in, got)
		}
	}
}

// A user row whose bubble may join two parts around a dropped `!command`
// matches every query; a `!` that cannot start such a part does not.
func TestFindCandidateTextOfUserRowAroundACommandIsAny(t *testing.T) {
	for _, summary := range []string{
		"a\n\n---\n\n!ls\n\n---\n\nb",
		"a\n\n---\n\n  !ls\n\n---\n\nb\n\n---\n\nc",
		"a\n\n![image](attachment://att-1)\n\n---\n\n!ls\n\n---\n\nb",
	} {
		if got := findCandidateText("user_text", summary, "{}", "", nil); got != findAnyCandidate {
			t.Errorf("candidate of %q = %q", summary, got)
		}
	}
	for _, summary := range []string{
		"Wow! !important",
		"!ls",
		"a\n\n---\n\n!ls",
		"!ls\n\n---\n\nb",
		"a\n\n---\n\nb !c\n\n---\n\nd",
	} {
		if got := findCandidateText("user_text", summary, "{}", "", nil); got == findAnyCandidate {
			t.Errorf("candidate of %q matches every query", summary)
		}
	}
}

// A row the filter cannot narrow matches every query.
func TestFindCandidateTextOfUnsureMarkdownIsAny(t *testing.T) {
	if got := findCandidateText("assistant_text", "[a [b](c) d](e) f", "{}", "", nil); got != findAnyCandidate {
		t.Fatalf("candidate = %q", got)
	}
	// Only Markdown rows are read as Markdown.
	if got := findCandidateText("user_text", "[a [b](c) d](e) f", "{}", "", nil); got != findLoose("[a [b](c) d](e) f") {
		t.Fatalf("user candidate = %q", got)
	}
	if got := findCandidateText("user_text", "see [a](b) x\n\n![image](attachment://att-1)\n\n---\n\nmore", "{}", "", nil); !strings.Contains(got, findLoose("see [a](b) x\n\n---\n\nmore")) {
		t.Fatalf("user candidate %q misses the text around an image block", got)
	}
}

func TestFindCandidateTextCoversWhatRowsShow(t *testing.T) {
	has := func(candidate, query string) bool {
		needle := findNeedle(query)
		return needle != "" && strings.Contains(candidate, needle)
	}
	md := findCandidateText("assistant_text", "Read [docs](https://example.com/x)suffix, **foo**bar and AT&amp;T.", "{}", "", nil)
	for _, query := range []string{"docssuffix", "docs suffix", "foobar", "AT&amp;T", "example"} {
		if !has(md, query) {
			t.Errorf("assistant candidate %q misses %q", md, query)
		}
	}
	tool := findCandidateText("tool_call", "Bash: ls", `{"input":{"command":"grep -rn needle-in-meta src/"}}`, "Bash", nil)
	if !has(tool, "needle-in-meta src") || !has(tool, "Bash") {
		t.Errorf("tool candidate %q misses its meta or tool name", tool)
	}
	payloadMeta := `{"command":"cargo test --payload"}`
	if card := findCandidateText("tool_call", "Bash", "{}", "Bash", &payloadMeta); !has(card, "cargo test --payload") {
		t.Errorf("tool candidate %q misses its payload meta", card)
	}
	if prose := findCandidateText("assistant_text", "plain", `{"input":{"command":"meta only"}}`, "", nil); has(prose, "meta only") {
		t.Errorf("assistant candidate %q holds its meta", prose)
	}
	content := "the first words of a long thought ... and its tail"
	thinking := findCandidateText("thinking", "and its tail", "{}", "", &content)
	if !has(thinking, "first words") || !has(thinking, "its tail") {
		t.Errorf("thinking candidate %q misses its content", thinking)
	}
	// The content variant comes last, so an appended chunk extends it.
	if !strings.HasSuffix(thinking, findLoose(content)) {
		t.Errorf("thinking candidate %q does not end with its content", thinking)
	}
	if got := findCandidateText("thinking", "tail only", "{}", "", nil); got != "tailonly" {
		t.Errorf("thinking candidate without content = %q", got)
	}
}

// A cell ending in "^" renders inside the cell above it, after its text.
func TestFindTableMergesJoinEachMarkedCellToTheOneAbove(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"| H | O |\n| --- | --- |\n| top | x |\n| merged ^ | y |\n| more ^ | z |", []string{"top merged", "top merged more"}},
		{"| H |\n| --- |\n| top |\n| ^ |", []string{"top"}},
		{"| H |\n| --- |\n| x^2^ |", nil},
		{"| a | b |\n| wide ||\n| c ^ | d |", []string{"wide c"}},
		{"| a | b |\n| one | two |\n| c ^ |", []string{"one c", "two c"}},
		{"plain ^ text | no table above", nil},
	} {
		if got := findTableMerges(tc.in); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("findTableMerges(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// findCandidateVectors.json holds, for rows of each kind, the runs of text
// the frontend's find text gives them (threadFindText.ts; its browser
// test holds the file to its projections). Every run's loose form must lie
// in the row's candidate text, or the candidate text matches every query:
// then every match the bar counts is in a candidate. The rows include every
// pair of a set of Markdown pieces and seeded sequences of them
// (markdownFindCorpus.ts findCandidateCombinations).
func TestFindCandidateTextHoldsEveryFrontendFindRun(t *testing.T) {
	raw, err := os.ReadFile("../../frontend/src/test/fixtures/findCandidateVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Rows []struct {
			Name     string   `json:"name"`
			Kind     string   `json:"kind"`
			Summary  string   `json:"summary"`
			Meta     string   `json:"meta"`
			ToolName string   `json:"toolName"`
			Payload  *string  `json:"payload"`
			Runs     []string `json:"runs"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Rows) == 0 {
		t.Fatal("no vectors")
	}
	runs, any := 0, 0
	for _, row := range vectors.Rows {
		candidate := findCandidateText(row.Kind, row.Summary, row.Meta, row.ToolName, row.Payload)
		if candidate == findAnyCandidate {
			any++
			continue
		}
		for _, run := range row.Runs {
			runs++
			if loose := findLoose(run); !strings.Contains(candidate, loose) {
				t.Errorf("%s: candidate %q misses run %q (loose %q)\nsummary %q", row.Name, candidate, run, loose, row.Summary)
			}
		}
	}
	if runs == 0 {
		t.Fatal("the vectors hold no runs")
	}
	t.Logf("%d rows, %d of them match every query", len(vectors.Rows), any)
}
