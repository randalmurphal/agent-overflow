package usermessage

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/store"
)

func TestLeadingCommandWord(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"bare command", "/workflow", "workflow"},
		{"command then instruction", "/workflow start the release run", "workflow"},
		{"command then newline", "/workflow\nstart it", "workflow"},
		{"command then tab", "/workflow\tstart it", "workflow"},
		{"hyphenated and numbered names parse", "/run-2 go", "run-2"},
		{"no slash", "workflow start", ""},
		{"slash not first", "please /workflow", ""},
		{"leading space is not a command", " /workflow", ""},
		{"bare slash", "/", ""},
		{"slash space", "/ workflow", ""},
		{"absolute path", "/tmp/scratch is where it went", ""},
		{"uppercase is not a command", "/Workflow", ""},
		{"digit-led name is not a command", "/2fast", ""},
		{"hyphen-led name is not a command", "/-x", ""},
		{"empty", "", ""},
		// The word must END at whitespace: a longer word that merely starts
		// with a command's name is a different word, and the registry lookup
		// must never see a prefix.
		{"longer word is its own word", "/workflows are nice", "workflows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LeadingCommandWord(tc.content); got != tc.want {
				t.Fatalf("LeadingCommandWord(%q) = %q, want %q", tc.content, got, tc.want)
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
