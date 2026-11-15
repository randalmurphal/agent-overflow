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

// The erased variant drops what renders no letters, outside code only.
func TestFindErasedKeepsCodeAndErasesMarkup(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[docs](https://example.com/x)suffix", "[docs]suffix"},
		{"[ref][label]x", "[ref]x"},
		{"see ![alt](a.png)now", "see now"},
		{"<b>foo</b>bar", "foobar"},
		{"a <!-- note --> b", "a  b"},
		{"AT&amp;T <b>x</b>", "AT&amp;T x"},
		{"`<b>` and <i>y</i>z", "`<b>` and yz"},
		{"``[a](b)`` [c](d)e", "``[a](b)`` [c]e"},
		{"```\n[a](b)<b>\n```\n[c](d)e", "```\n[a](b)<b>\n```\n[c]e"},
		{"plain text", "plain text"},
		{"<https://example.com> <user@example.com>", "<https://example.com> <user@example.com>"},
	} {
		if got := findErased(tc.in); got != tc.want {
			t.Errorf("findErased(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
// in the row's candidate text: then every match the bar counts is in a
// candidate.
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
	runs := 0
	for _, row := range vectors.Rows {
		candidate := findCandidateText(row.Kind, row.Summary, row.Meta, row.ToolName, row.Payload)
		for _, run := range row.Runs {
			runs++
			if loose := findLoose(run); !strings.Contains(candidate, loose) {
				t.Errorf("%s: candidate %q misses run %q (loose %q)", row.Name, candidate, run, loose)
			}
		}
	}
	if runs == 0 {
		t.Fatal("the vectors hold no runs")
	}
}
