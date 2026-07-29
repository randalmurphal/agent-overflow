package usermessage

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/store"
)

func TestCommandWords(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"bare command", "/workflow", []string{"workflow"}},
		{"command then instruction", "/workflow start the release run", []string{"workflow"}},
		{"command then newline", "/workflow\nstart it", []string{"workflow"}},
		{"command then tab", "/workflow\tstart it", []string{"workflow"}},
		{"hyphenated and numbered names parse", "/run-2 go", []string{"run-2"}},
		// D31 as amended: a command word counts at ANY word position, not only
		// the first. Start of a line, mid-sentence, after a newline.
		{"mid sentence", "please run /workflow now", []string{"workflow"}},
		{"leading space", " /workflow", []string{"workflow"}},
		{"after a newline", "do this:\n/workflow", []string{"workflow"}},
		{"end of text", "and then /workflow", []string{"workflow"}},
		{"repeated, in order, duplicates kept", "/workflow then /workflow", []string{"workflow", "workflow"}},
		{"unregistered shapes are still shapes, in order", "check /tmp then /workflow", []string{"tmp", "workflow"}},
		{"no slash", "workflow start", nil},
		{"bare slash", "/", nil},
		{"slash space", "/ workflow", nil},
		{"path is one word and not a command", "/tmp/scratch is where it went", nil},
		{"path mid sentence is not a command", "see /tmp/scratch/workflow for it", nil},
		{"uppercase is not a command", "/Workflow", nil},
		{"digit-led name is not a command", "/2fast", nil},
		{"hyphen-led name is not a command", "/-x", nil},
		{"trailing punctuation is part of the word", "run /workflow, then stop", nil},
		{"empty", "", nil},
		// The word must END at whitespace: a longer word that merely starts
		// with a command's name is a different word, and the registry lookup
		// must never see a prefix.
		{"longer word is its own word", "/workflows are nice", []string{"workflows"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandWords(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("CommandWords(%q) = %q, want %q", tc.content, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("CommandWords(%q) = %q, want %q", tc.content, got, tc.want)
				}
			}
		})
	}
}

// TestCommandWordsPicksTheFirstRegistered mirrors, case for case, the
// `slashCommandMatches` table in
// `frontend/src/lib/components/composer/slashCommands.test.ts`. The two
// matchers are parallel by hand, so the tables are the parity check: a change
// on one side that is not made on the other shows up as a failing twin.
func TestCommandWordsPicksTheFirstRegistered(t *testing.T) {
	registered := map[string]bool{"workflow": true}
	first := func(content string) string {
		for _, name := range CommandWords(content) {
			if registered[name] {
				return name
			}
		}
		return ""
	}
	cases := []struct {
		content string
		want    string
	}{
		{"/workflow", "workflow"},
		{"/workflow start the release", "workflow"},
		{"/workflow\nstart the release", "workflow"},
		{"/workflow\tstart", "workflow"},
		{"ask about /workflow later", "workflow"},
		{" /workflow", "workflow"},
		{"line one\n/workflow", "workflow"},
		{"first /tmp then /workflow", "workflow"},
		{"/workflow and again /workflow", "workflow"},
		{"/workflows are nice", ""},
		{"/Workflow", ""},
		{"/", ""},
		{"/ workflow", ""},
		{"/tmp/scratch has the log", ""},
		{"see /tmp/scratch/workflow", ""},
		{"a/workflow is not a word start", ""},
		{"workflow", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.content, func(t *testing.T) {
			if got := first(tc.content); got != tc.want {
				t.Fatalf("first registered of %q = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestMarshalCarriesCommandAlone(t *testing.T) {
	// A `/workflow` send with no attachments and no plan context still needs
	// a meta row: the marker is the only record that an expansion happened.
	got, err := Marshal(Input{Command: "workflow"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Meta
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if decoded.Command != "workflow" {
		t.Fatalf("Command = %q, want %q (raw: %s)", decoded.Command, "workflow", got)
	}
	// Round-trips through the item column the frontend reads.
	back, err := FromItem(store.Item{Meta: got})
	if err != nil {
		t.Fatalf("FromItem: %v", err)
	}
	if back.Command != "workflow" {
		t.Fatalf("round-tripped Command = %q", back.Command)
	}
}

func TestMarshalOmitsCommandWhenAbsent(t *testing.T) {
	got, err := Marshal(Input{Attachments: []store.Attachment{
		{ID: "a", ThreadID: "t", Filename: "f.png", MimeType: "image/png", Size: 1},
	}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(got), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["command"]; present {
		t.Fatalf("command key leaked into a message that invoked none: %s", got)
	}
}
