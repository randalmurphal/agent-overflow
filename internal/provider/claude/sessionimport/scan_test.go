package sessionimport

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractJSONStringField(t *testing.T) {
	cases := []struct {
		name, text, key, want string
	}{
		{"compact spelling", `{"cwd":"/repo","x":1}`, "cwd", "/repo"},
		{"spaced spelling", `{"cwd": "/repo"}`, "cwd", "/repo"},
		{"first occurrence wins", `{"cwd":"/one"}` + "\n" + `{"cwd":"/two"}`, "cwd", "/one"},
		{"escaped quote", `{"t":"say \"hi\" now"}`, "t", `say "hi" now`},
		{"escaped newline", `{"t":"line1\nline2"}`, "t", "line1\nline2"},
		{"escaped backslash before quote", `{"t":"path\\"}`, "t", `path\`},
		{"absent key", `{"other":"x"}`, "cwd", ""},
		{"truncated value", `{"cwd":"/repo-that-got-cut`, "cwd", ""},
		{"non-string value ignored", `{"cwd":123}`, "cwd", ""},
		// Both spellings in one window: the answer is the one that comes
		// first in the BUFFER, not the one whose spelling is tried first.
		// A window routinely holds records written by two CLI versions.
		{"spaced first, compact later", `{"cwd": "/one"}` + "\n" + `{"cwd":"/two"}`, "cwd", "/one"},
		{"compact first, spaced later", `{"cwd":"/one"}` + "\n" + `{"cwd": "/two"}`, "cwd", "/one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSONStringField([]byte(tc.text), tc.key); got != tc.want {
				t.Errorf("extractJSONStringField = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractLastJSONStringField(t *testing.T) {
	cases := []struct {
		name, text, key, want string
	}{
		{"last of many", `{"t":"one"}` + "\n" + `{"t":"two"}` + "\n" + `{"t":"three"}`, "t", "three"},
		{"mixed spellings", `{"t":"one"}` + "\n" + `{"t": "two"}`, "t", "two"},
		{"skips a truncated trailing value", `{"t":"good"}` + "\n" + `{"t":"cut off`, "t", "good"},
		{"absent", `{"other":"x"}`, "t", ""},
		{"escapes decoded", `{"t":"a"}` + "\n" + `{"t":"b\tc"}`, "t", "b\tc"},
		// The mirror of the first-by-position rule: the newest record wins
		// whichever spelling it happens to use. Scanning one spelling to
		// exhaustion before the other would hand it to the older record.
		{"spaced record is the newest", `{"t":"old"}` + "\n" + `{"t": "new"}`, "t", "new"},
		{"compact record is the newest", `{"t": "old"}` + "\n" + `{"t":"new"}`, "t", "new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractLastJSONStringField([]byte(tc.text), tc.key); got != tc.want {
				t.Errorf("extractLastJSONStringField = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnescapeJSONStringKeepsMalformedInput(t *testing.T) {
	if got := unescapeJSONString([]byte(`no escapes`)); got != "no escapes" {
		t.Errorf("got %q", got)
	}
	if got := unescapeJSONString([]byte(`bad \q escape`)); got != `bad \q escape` {
		t.Errorf("malformed escape must round-trip verbatim, got %q", got)
	}
}

func TestExtractFirstPromptFromHead(t *testing.T) {
	line := func(parts ...string) string { return strings.Join(parts, "\n") }

	cases := []struct {
		name, head, want string
	}{
		{
			"plain prompt",
			`{"type":"user","message":{"role":"user","content":"hello there"}}`,
			"hello there",
		},
		{
			// The title preview takes the FIRST usable text block, matching
			// the CLI's own listing. (Message conversion joins them —
			// different job, see userPromptText.)
			"first usable text block wins",
			`{"type":"user","message":{"content":[{"type":"text","text":"  "},{"type":"text","text":"second"}]}}`,
			"second",
		},
		{
			"newlines collapse to spaces",
			`{"type":"user","message":{"content":"first\nsecond"}}`,
			"first second",
		},
		{
			"tool results are skipped",
			line(
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"meta rows are skipped",
			line(
				`{"type":"user","isMeta":true,"message":{"content":"caveat"}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"compaction summaries are skipped",
			line(
				`{"type":"user","isCompactSummary":true,"message":{"content":"summary"}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"wrapper tags are skipped",
			line(
				`{"type":"user","message":{"content":"<ide-context>files</ide-context>"}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"interrupt markers are skipped",
			line(
				`{"type":"user","message":{"content":"[Request interrupted by user for tool use]"}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"bash input keeps its bang prefix",
			`{"type":"user","message":{"content":"<bash-input>git status</bash-input>"}}`,
			"! git status",
		},
		{
			"slash command is only a fallback",
			line(
				`{"type":"user","message":{"content":"<command-name>compact</command-name>"}}`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{
			"slash command used when nothing else exists",
			`{"type":"user","message":{"content":"<command-name>compact</command-name>"}}`,
			"compact",
		},
		{
			"unparseable lines are skipped",
			line(
				`{"type":"user", BROKEN`,
				`{"type":"user","message":{"content":"the real prompt"}}`,
			),
			"the real prompt",
		},
		{"nothing at all", `{"type":"assistant","message":{"content":[]}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractFirstPromptFromHead([]byte(tc.head)); got != tc.want {
				t.Errorf("extractFirstPromptFromHead = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractFirstPromptTruncates(t *testing.T) {
	long := strings.Repeat("é", 260)
	head := `{"type":"user","message":{"content":"` + long + `"}}`
	got := extractFirstPromptFromHead([]byte(head))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long prompt not truncated: %q", got)
	}
	if runes := []rune(got); len(runes) != maxFirstPromptRunes+1 {
		t.Errorf("got %d runes, want %d + ellipsis", len(runes), maxFirstPromptRunes)
	}
}

func TestValidSessionUUID(t *testing.T) {
	if !validSessionUUID("0558cd5c-7f13-47f9-b7f5-d56ccce08f9c") {
		t.Error("a real session id was rejected")
	}
	for _, bad := range []string{"", "memory", "0558cd5c7f1347f9b7f5d56ccce08f9c", "0558cd5c-7f13-47f9-b7f5-d56ccce08f9c-extra"} {
		if validSessionUUID(bad) {
			t.Errorf("%q was accepted as a session id", bad)
		}
	}
}

func TestReadLite(t *testing.T) {
	dir := t.TempDir()

	t.Run("small file reads head as tail", func(t *testing.T) {
		path := filepath.Join(dir, "small.jsonl")
		if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		lite, err := readLite(path, make([]byte, liteBufSize))
		if err != nil {
			t.Fatalf("readLite: %v", err)
		}
		if !bytes.Equal(lite.Head, lite.Tail) {
			t.Errorf("small file head != tail")
		}
		if lite.Size != 18 {
			t.Errorf("size = %d, want 18", lite.Size)
		}
	})

	t.Run("large file splits head and tail", func(t *testing.T) {
		path := filepath.Join(dir, "large.jsonl")
		body := "HEAD-MARKER" + strings.Repeat("x", liteReadBufSize*2) + "TAIL-MARKER"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		lite, err := readLite(path, make([]byte, liteBufSize))
		if err != nil {
			t.Fatalf("readLite: %v", err)
		}
		if !bytes.HasPrefix(lite.Head, []byte("HEAD-MARKER")) {
			t.Error("head window missed the start of the file")
		}
		if !bytes.HasSuffix(lite.Tail, []byte("TAIL-MARKER")) {
			t.Error("tail window missed the end of the file")
		}
		// Head and tail must be DISJOINT regions of the worker buffer: a
		// single window reused for both would leave Head holding the tail.
		if &lite.Head[0] == &lite.Tail[0] {
			t.Error("head and tail alias the same window")
		}
		if len(lite.Head) != liteReadBufSize || len(lite.Tail) != liteReadBufSize {
			t.Errorf("window sizes = %d / %d, want %d", len(lite.Head), len(lite.Tail), liteReadBufSize)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.jsonl")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := readLite(path, make([]byte, liteBufSize)); err == nil {
			t.Error("empty file: want an error")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := readLite(filepath.Join(dir, "nope"), make([]byte, liteBufSize)); err == nil {
			t.Error("missing file: want an error")
		}
	})
}
