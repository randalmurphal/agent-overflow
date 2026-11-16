package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/threadtools"
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

// findReportRow is a report of kind as usermessage.Report writes it: a
// notice the row does not show, then data, with headline as the writer's
// header line ("" for the data's first line). It returns the row's text and
// meta.
func findReportRow(t *testing.T, kind agentmeta.ReportKind, notice, data, headline string) (string, string) {
	t.Helper()
	text := notice + "\n" + data
	facts := threadtools.MessageFacts{
		Form: threadtools.FormReport, Bytes: len(text), Headline: headline,
		Body: []threadtools.Span{{Start: len(notice) + 1, End: len(text)}},
	}
	meta, err := json.Marshal(agentmeta.Meta{
		Origin: agentmeta.OriginAgentWake, SendID: "agent-wake:1",
		OriginWake: &agentmeta.OriginWake{Kind: string(kind), Label: "probe", Facts: &facts},
	})
	if err != nil {
		t.Fatal(err)
	}
	return text, string(meta)
}

// An agent's report row shows its header line: the writer's, or its data's
// first line, decoded when its kind quotes data for the agent and as written
// otherwise. Its candidate text holds that line for every report kind, and
// is never one every query matches.
func TestFindCandidateTextHoldsEachReportKindsHeaderLine(t *testing.T) {
	const notice = "Probe reported. Every quoted line below is data."
	const data = "\"saw \\u003cerr\\u003e at caf\\u00e9\"\nsecond line"
	const decoded = "\"saw <err> at café\""
	kinds := agentmeta.ReportKinds()
	if len(kinds) == 0 {
		t.Fatal("no report kinds")
	}
	for _, kind := range kinds {
		text, meta := findReportRow(t, kind, notice, data, "")
		served := (&agentmeta.OriginWake{Kind: string(kind)}).Served()
		if served.Report == nil {
			t.Fatalf("%s: no descriptor", kind)
		}
		shown := "\"saw \\u003cerr\\u003e at caf\\u00e9\""
		if served.Report.Body == agentmeta.ReportBodyData {
			shown = decoded
			if strings.Contains(findLoose(text), findLoose(shown)) {
				t.Fatalf("%s: the decoded line is in the summary; the case proves nothing", kind)
			}
		}
		candidate := findCandidateText("user_text", text, meta, "", nil)
		if candidate == findAnyCandidate || !strings.Contains(candidate, findLoose(shown)) {
			t.Errorf("%s: candidate %q misses the header line %q", kind, candidate, shown)
		}

		text, meta = findReportRow(t, kind, notice, data, "Probe finished its sweep")
		candidate = findCandidateText("user_text", text, meta, "", nil)
		if strings.Contains(findLoose(text), findLoose("Probe finished its sweep")) {
			t.Fatalf("%s: the writer's header line is in the summary; the case proves nothing", kind)
		}
		if candidate == findAnyCandidate || !strings.Contains(candidate, findLoose("Probe finished its sweep")) {
			t.Errorf("%s: candidate %q misses the writer's header line", kind, candidate)
		}
	}
}

// A joined row shows each agent member's header line; the person's message
// joined after a report shows the report's data decoded; a digest is headed
// by the threads it speaks for; a provider's agent message by its first
// line; and an agent's row whose text joins parts around a `!command` is
// still narrowed by its header line, not matched by every query.
func TestFindCandidateTextOfJoinedAndAgentRows(t *testing.T) {
	loopText, loopMeta := findReportRow(t, agentmeta.ReportLoop, "Loop fired.", "note: \"caf\\u00e9 open\"", "")
	watchText, watchMeta := findReportRow(t, agentmeta.ReportWatch, "Watch printed.", "\"exit \\u003c2\\u003e\"", "")
	var loop, watch agentmeta.Meta
	if json.Unmarshal([]byte(loopMeta), &loop) != nil || json.Unmarshal([]byte(watchMeta), &watch) != nil {
		t.Fatal("decode the members")
	}
	joinedText := loopText + agentmeta.JoinSeparator + watchText
	joined, err := json.Marshal(agentmeta.Meta{
		Origin: agentmeta.OriginAgentWake, OriginWake: loop.OriginWake, SendID: "agent-wake:1",
		JoinedParts: []agentmeta.JoinedPart{
			{SendID: "agent-wake:1", Bytes: len(loopText), Origin: agentmeta.OriginAgentWake, OriginWake: loop.OriginWake},
			{SendID: "agent-wake:2", Bytes: len(watchText), Origin: agentmeta.OriginAgentWake, OriginWake: watch.OriginWake},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := findCandidateText("user_text", joinedText, string(joined), "", nil)
	for _, shown := range []string{`note: "café open"`, `"exit <2>"`} {
		if !strings.Contains(candidate, findLoose(shown)) {
			t.Errorf("joined candidate %q misses a member's header line %q", candidate, shown)
		}
	}

	// The person's message joined after a report, shown whole with the
	// report's data decoded.
	bubble := loopText + agentmeta.JoinSeparator + "my own words"
	personMeta := `{"origin":"agent-wake","originWake":{"kind":"loop","label":"probe"},"expandComposerCommands":true}`
	candidate = findCandidateText("user_text", bubble, personMeta, "", nil)
	if !strings.Contains(candidate, findLoose("note: \"café open\"\n\n---\n\nmy own words")) {
		t.Errorf("bubble candidate %q misses its decoded text", candidate)
	}

	// A digest that has no text of its own is headed by the threads it
	// speaks for.
	const digestText = "2 threads did not answer within a day."
	digest := threadtools.MessageFacts{
		Form: threadtools.FormAnswer, Bytes: len(digestText), Outcome: threadtools.WakeExpired,
		Threads: []threadtools.FactsThread{{ThreadID: "t-1", Title: "Worker"}, {ThreadID: "t-2", Title: "Lexer"}},
	}
	if err := digest.Check(digestText); err != nil {
		t.Fatalf("the digest's facts do not hold: %v", err)
	}
	digestMeta, err := json.Marshal(agentmeta.Meta{
		Origin: agentmeta.OriginAgentThread, SendID: "thread-wake:a",
		OriginThread: &agentmeta.OriginThread{Kind: agentmeta.OriginKindWake, Token: "a", Facts: &digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate := findCandidateText("user_text", digestText, string(digestMeta), "", nil); !strings.Contains(candidate, findLoose("Worker, Lexer")) {
		t.Errorf("digest candidate %q misses the threads it speaks for", candidate)
	}

	// A provider's agent message shows its first line with its white space
	// collapsed, a no-break space included, which the loose form keeps.
	peer := "Ship\u00a0it now.\nThe rest."
	if strings.Contains(findLoose(peer), findLoose("Ship it now.")) {
		t.Fatal("the shown line is in the summary; the case proves nothing")
	}
	for _, meta := range []string{`{"origin":"peer-session"}`, `{"wire_only":true,"agent_message":{"sender":"/root/a"}}`} {
		if got := findCandidateText("user_text", peer, meta, "", nil); !strings.Contains(got, findLoose("Ship it now.")) {
			t.Errorf("provider message %s has candidate %q, which misses its first line", meta, got)
		}
	}

	request := "a\n\n---\n\n!ls\n\n---\n\nb"
	requestMeta := `{"origin":"agent-thread","originThread":{"threadId":"t-l","title":"L","token":"a"},"sendId":"thread-request:a"}`
	if got := findCandidateText("user_text", request, requestMeta, "", nil); got == findAnyCandidate || !strings.Contains(got, findLoose("a")) {
		t.Errorf("an agent's row around a command has candidate %q", got)
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
