package memory

import (
	"strings"
	"testing"
)

func TestValidateDraftRefusesAnUnknownKind(t *testing.T) {
	// The closed vocabulary is what makes the log greppable and the digest
	// groupable. A near-miss must be refused, not filed under itself.
	for _, kind := range []string{"", "Pattern", "note", "ruling", "insight"} {
		findings := ValidateDraft(Draft{Kind: kind, Text: "something"})
		if len(findings) != 1 || findings[0].Path != ".kind" {
			t.Fatalf("kind %q findings = %v, want exactly one on .kind", kind, findings)
		}
		if !strings.Contains(findings[0].Message, KindList()) {
			t.Fatalf("kind %q refusal does not name the vocabulary: %s", kind, findings[0].Message)
		}
	}
	for _, kind := range Kinds {
		if findings := ValidateDraft(Draft{Kind: kind, Text: "something"}); len(findings) != 0 {
			t.Fatalf("kind %q was refused: %v", kind, findings)
		}
	}
}

// `ruling` is deliberately deferred. If it is ever added it needs its own scope
// conversation, so this test exists to make adding it a conscious edit.
func TestRulingIsNotAKind(t *testing.T) {
	if KnownKind("ruling") {
		t.Fatal("`ruling` is in the vocabulary; it is deferred pending its own scope conversation")
	}
}

func TestValidateDraftEnforcesBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		draft Draft
		path  string
	}{
		"blank text":    {Draft{Kind: KindWarning, Text: "   \n "}, ".text"},
		"oversize text": {Draft{Kind: KindWarning, Text: strings.Repeat("x", MaxTextBytes+1)}, ".text"},
		"too many files": {
			Draft{Kind: KindWarning, Text: "ok", Files: make([]string, MaxFiles+1)}, ".files"},
		"blank file":   {Draft{Kind: KindWarning, Text: "ok", Files: []string{" "}}, ".files[0]"},
		"long file":    {Draft{Kind: KindWarning, Text: "ok", Files: []string{strings.Repeat("p", MaxFilePathBytes+1)}}, ".files[0]"},
		"control file": {Draft{Kind: KindWarning, Text: "ok", Files: []string{"a\nb.go"}}, ".files[0]"},
	} {
		findings := ValidateDraft(tc.draft)
		found := false
		for _, finding := range findings {
			found = found || finding.Path == tc.path
		}
		if !found {
			t.Errorf("%s: findings %v carry nothing on %s", name, findings, tc.path)
		}
	}
	// A note at exactly the cap is legal: the bound is a ceiling, not a target.
	if findings := ValidateDraft(Draft{
		Kind: KindLearning, Text: strings.Repeat("x", MaxTextBytes), Files: make([]string, MaxFiles),
	}); len(findings) != MaxFiles {
		// Every one of the MaxFiles empty strings is its own finding; nothing on
		// .text or .files itself.
		for _, finding := range findings {
			if finding.Path == ".text" || finding.Path == ".files" {
				t.Fatalf("value at the cap was refused: %v", finding)
			}
		}
	}
}

func TestValidateDraftCollectsEveryFinding(t *testing.T) {
	// One retry turn has to see everything wrong with what it sent, so findings
	// are collected rather than short-circuited.
	findings := ValidateDraft(Draft{Kind: "bogus", Text: "", Files: []string{""}})
	paths := map[string]bool{}
	for _, finding := range findings {
		paths[finding.Path] = true
	}
	for _, want := range []string{".kind", ".text", ".files[0]"} {
		if !paths[want] {
			t.Fatalf("findings %v are missing %s", findings, want)
		}
	}
}

// NewNote is the ONLY constructor, and provenance is a parameter rather than a
// draft field. That is what makes an author-supplied provenance structurally
// impossible rather than merely filtered.
func TestNewNoteStampsProvenanceAndRefusesAnInvalidDraft(t *testing.T) {
	provenance := Provenance{RunID: "run-1", PhaseID: "implement", Attempt: 2, Wave: 3}
	note, err := NewNote(Draft{Kind: KindPattern, Text: "  keep it  "}, provenance, 1700)
	if err != nil {
		t.Fatal(err)
	}
	if note.Text != "keep it" {
		t.Fatalf("text = %q, want it trimmed", note.Text)
	}
	if note.Provenance != provenance || note.At != 1700 {
		t.Fatalf("note = %+v, want the supplied provenance and timestamp", note)
	}
	if _, err := NewNote(Draft{Kind: "bogus", Text: "x"}, provenance, 1); err == nil {
		t.Fatal("an invalid draft became a note")
	} else if !strings.Contains(err.Error(), ".kind") {
		t.Fatalf("error = %v, want it to name .kind", err)
	}
}
