package discussion

import (
	"strings"
	"testing"
)

func TestParseConclusionProposal(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantSummary string
		wantOK      bool
	}{
		{
			name:        "marker on last line with summary",
			input:       "Here's my final take.\nCONCLUDE: we agree on the migration boundary.",
			wantSummary: "we agree on the migration boundary.",
			wantOK:      true,
		},
		{
			name:        "lowercase marker",
			input:       "conclude: all set.",
			wantSummary: "all set.",
			wantOK:      true,
		},
		{
			name:        "mixed-case marker",
			input:       "Conclude: mixed case works too.",
			wantSummary: "mixed case works too.",
			wantOK:      true,
		},
		{
			name:        "leading whitespace before marker",
			input:       "Final thought.\n   CONCLUDE: indented marker.",
			wantSummary: "indented marker.",
			wantOK:      true,
		},
		{
			name:        "marker mid-text with trailing prose after it is not a proposal",
			input:       "CONCLUDE: I think we're done.\nActually, one more thing.",
			wantSummary: "",
			wantOK:      false,
		},
		{
			name:        "empty summary is still a proposal",
			input:       "CONCLUDE:",
			wantSummary: "",
			wantOK:      true,
		},
		{
			name:        "whitespace-only summary is still empty-but-ok",
			input:       "CONCLUDE:    ",
			wantSummary: "",
			wantOK:      true,
		},
		{
			name:        "CRLF line endings tolerated",
			input:       "First line.\r\nCONCLUDE: done via CRLF.\r\n",
			wantSummary: "done via CRLF.",
			wantOK:      true,
		},
		{
			name:        "empty input",
			input:       "",
			wantSummary: "",
			wantOK:      false,
		},
		{
			name:        "whitespace-only input",
			input:       "   \n\t\n  ",
			wantSummary: "",
			wantOK:      false,
		},
		{
			name:        "no marker at all",
			input:       "Just a normal reply with no marker.",
			wantSummary: "",
			wantOK:      false,
		},
		{
			name:        "marker word appears but not as a line prefix",
			input:       "We should CONCLUDE: soon but not yet.",
			wantSummary: "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSummary, gotOK := ParseConclusionProposal(tt.input)
			if gotOK != tt.wantOK {
				t.Fatalf("ParseConclusionProposal(%q) ok = %v, want %v", tt.input, gotOK, tt.wantOK)
			}
			if gotSummary != tt.wantSummary {
				t.Fatalf("ParseConclusionProposal(%q) summary = %q, want %q", tt.input, gotSummary, tt.wantSummary)
			}
		})
	}
}

func TestParseConclusionProposalTruncatesLongSummaryRuneSafe(t *testing.T) {
	// 600 multi-byte runes (each 'é' is 2 bytes in UTF-8) — truncation
	// must count runes, not bytes, and must not split the final rune.
	longSummary := strings.Repeat("é", 600)
	input := "CONCLUDE: " + longSummary

	gotSummary, ok := ParseConclusionProposal(input)
	if !ok {
		t.Fatal("expected ok=true for a long summary")
	}
	gotRuneCount := len([]rune(gotSummary))
	if gotRuneCount != conclusionSummaryMaxRunes {
		t.Fatalf("truncated summary rune count = %d, want %d", gotRuneCount, conclusionSummaryMaxRunes)
	}
	if !strings.HasPrefix(gotSummary, strings.Repeat("é", 10)) {
		t.Fatalf("truncated summary = %q, want it to start with the original content", gotSummary)
	}
	// Every rune must still be a valid 'é' — a byte-based truncation
	// would corrupt the boundary rune into invalid UTF-8 or a mojibake
	// replacement character.
	for i, r := range gotSummary {
		if r != 'é' {
			t.Fatalf("rune at byte offset %d = %q, want 'é' (truncation split a multi-byte rune)", i, r)
		}
	}
}

func TestBuildConclusionMessageTurnLimitForm(t *testing.T) {
	got := BuildConclusionMessage(ConclusionMessageInput{
		Unanimous: false,
		MaxTurns:  8,
	})
	want := "Discussion concluded: reached the 8-turn limit."
	if got != want {
		t.Fatalf("BuildConclusionMessage(turn-limit) = %q, want %q", got, want)
	}
}

func TestBuildConclusionMessageUnanimousWithTwoRolesInRosterOrder(t *testing.T) {
	got := BuildConclusionMessage(ConclusionMessageInput{
		Unanimous:           true,
		MaxTurns:            8,
		ParticipantsInOrder: []string{"thread-a", "thread-b"},
		Proposals: map[string]string{
			"thread-a": "the migration boundary is settled",
			"thread-b": "agreed, no further concerns",
		},
		RoleByThreadID: map[string]string{
			"thread-a": "Architect",
			"thread-b": "Reviewer",
		},
	})
	want := "Discussion concluded: all participants proposed to conclude.\n\n" +
		"Architect: the migration boundary is settled\n" +
		"Reviewer: agreed, no further concerns"
	if got != want {
		t.Fatalf("BuildConclusionMessage(unanimous) = %q, want %q", got, want)
	}
}

func TestBuildConclusionMessageUnanimousSkipsEmptySummaryParticipant(t *testing.T) {
	got := BuildConclusionMessage(ConclusionMessageInput{
		Unanimous:           true,
		MaxTurns:            8,
		ParticipantsInOrder: []string{"thread-a", "thread-b"},
		Proposals: map[string]string{
			"thread-a": "",
			"thread-b": "agreed, no further concerns",
		},
		RoleByThreadID: map[string]string{
			"thread-a": "Architect",
			"thread-b": "Reviewer",
		},
	})
	want := "Discussion concluded: all participants proposed to conclude.\n\n" +
		"Reviewer: agreed, no further concerns"
	if got != want {
		t.Fatalf("BuildConclusionMessage(skip-empty) = %q, want %q", got, want)
	}
	if strings.Contains(got, "Architect") {
		t.Fatalf("BuildConclusionMessage(skip-empty) = %q, want no Architect line (empty summary)", got)
	}
}

func TestBuildConclusionMessageUnanimousAllEmptySummariesOmitsBlock(t *testing.T) {
	got := BuildConclusionMessage(ConclusionMessageInput{
		Unanimous:           true,
		MaxTurns:            8,
		ParticipantsInOrder: []string{"thread-a", "thread-b"},
		Proposals: map[string]string{
			"thread-a": "",
			"thread-b": "   ",
		},
		RoleByThreadID: map[string]string{
			"thread-a": "Architect",
			"thread-b": "Reviewer",
		},
	})
	want := "Discussion concluded: all participants proposed to conclude."
	if got != want {
		t.Fatalf("BuildConclusionMessage(all-empty) = %q, want %q (no trailing summaries block)", got, want)
	}
}
