package terminal

import (
	"bytes"
	"testing"
)

func TestStripReplayableQueries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Device Attributes — vim sends DA1 on startup, and the reply a
		// hydrating xterm produces ("\x1b[?64;...c") lands at the prompt.
		{"primary DA bare", "a\x1b[cb", "ab"},
		{"primary DA with param", "a\x1b[0cb", "ab"},
		{"secondary DA", "a\x1b[>c" + "b", "ab"},
		{"secondary DA with param", "a\x1b[>0cb", "ab"},
		{"tertiary DA", "a\x1b[=cb", "ab"},

		// DSR / cursor position reports.
		{"DSR status", "a\x1b[5nb", "ab"},
		{"DSR cursor position", "a\x1b[6nb", "ab"},
		{"DEC DSR cursor position", "a\x1b[?6nb", "ab"},
		{"DEC DSR printer", "a\x1b[?15nb", "ab"},

		// DECRQM / DECRPM — the "$" intermediate is the discriminator.
		{"DECRQM DEC mode", "a\x1b[?2026$pb", "ab"},
		{"DECRQM ANSI mode", "a\x1b[4$pb", "ab"},
		{"DECRPM reply", "a\x1b[?2026;1$yb", "ab"},
		{"DECSTR kept", "a\x1b[!pb", "a\x1b[!pb"},
		{"DECSCL kept", "a\x1b[62;1\"pb", "a\x1b[62;1\"pb"},

		// XTVERSION vs DECSCUSR.
		{"XTVERSION bare", "a\x1b[>qb", "ab"},
		{"XTVERSION with param", "a\x1b[>0qb", "ab"},
		{"DECSCUSR kept", "a\x1b[4 qb", "a\x1b[4 qb"},

		// kitty keyboard protocol — only the "?" query goes; the
		// stateful push/pop/set forms and restore-cursor must replay.
		{"kitty query", "a\x1b[?ub", "ab"},
		{"restore cursor kept", "a\x1b[ub", "a\x1b[ub"},
		{"kitty push kept", "a\x1b[>1ub", "a\x1b[>1ub"},
		{"kitty pop kept", "a\x1b[<ub", "a\x1b[<ub"},
		{"kitty set kept", "a\x1b[=1;1ub", "a\x1b[=1;1ub"},

		// DCS queries and replies.
		{"DECRQSS query", "a\x1bP$qm\x1b\\b", "ab"},
		{"XTGETTCAP query", "a\x1bP+q544e\x1b\\b", "ab"},
		{"DECRQSS reply", "a\x1bP1$r0m\x1b\\b", "ab"},
		{"XTGETTCAP failure reply", "a\x1bP0+r\x1b\\b", "ab"},
		{"sixel kept", "a\x1bP0;0;0q#0;2;0;0;0~\x1b\\b", "a\x1bP0;0;0q#0;2;0;0;0~\x1b\\b"},
		{"DECUDK kept", "a\x1bP1;1|17/1b\x1b\\b", "a\x1bP1;1|17/1b\x1b\\b"},

		// OSC color queries — neovim and fish probe the background.
		{"OSC 11 query BEL", "a\x1b]11;?\x07b", "ab"},
		{"OSC 10 query ST", "a\x1b]10;?\x1b\\b", "ab"},
		{"OSC 4 palette query", "a\x1b]4;1;?\x07b", "ab"},
		{"OSC 12 cursor query", "a\x1b]12;?\x07b", "ab"},
		{"OSC color set kept", "a\x1b]11;#000000\x07b", "a\x1b]11;#000000\x07b"},
		{"OSC 4 palette set kept", "a\x1b]4;1;rgb:00/00/00\x07b", "a\x1b]4;1;rgb:00/00/00\x07b"},
		{"OSC title kept", "a\x1b]0;my title?\x07b", "a\x1b]0;my title?\x07b"},
		{"OSC numeric title kept", "a\x1b]0;123;?\x07b", "a\x1b]0;123;?\x07b"},
		{"OSC 52 clipboard kept", "a\x1b]52;c;YQ==\x07b", "a\x1b]52;c;YQ==\x07b"},

		// Ordinary content must be byte-identical.
		{"plain text", "hello world", "hello world"},
		{"color and cursor sequences kept", "\x1b[31mred\x1b[0m\x1b[2J\x1b[H", "\x1b[31mred\x1b[0m\x1b[2J\x1b[H"},
		{"utf8 kept", "héllo — 日本語 🎉", "héllo — 日本語 🎉"},
		{"multiple queries in one stream", "\x1b[6nfoo\x1b[?u bar\x1b]11;?\x07baz\x1b[c", "foo barbaz"},

		// Truncation edges: an incomplete sequence at the buffer end is
		// passed through, never mis-stripped and never lost.
		{"bare trailing ESC", "abc\x1b", "abc\x1b"},
		{"incomplete CSI at end", "abc\x1b[?", "abc\x1b[?"},
		{"incomplete CSI query at end", "abc\x1b[6", "abc\x1b[6"},
		{"unterminated DCS query at end", "abc\x1bP$qm", "abc\x1bP$qm"},
		{"unterminated OSC query at end", "abc\x1b]11;?", "abc\x1b]11;?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripReplayableQueries([]byte(tc.in))
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("stripReplayableQueries(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The 8-bit C1 singles (0x90 DCS, 0x9C ST, 0x9D OSC) are UTF-8
// continuation bytes; the stripper must never treat them as sequence
// introducers, or multibyte text would be corrupted.
func TestStripReplayableQueriesLeavesEightBitC1Alone(t *testing.T) {
	// "А" (А) encodes as 0xD0 0x90 — the second byte is C1 DCS.
	in := []byte("Аќѝ plain")
	got := stripReplayableQueries(in)
	if !bytes.Equal(got, in) {
		t.Fatalf("stripReplayableQueries(%q) = %q, want unchanged", in, got)
	}
}
