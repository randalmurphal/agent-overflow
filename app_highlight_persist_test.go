package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/highlight"
)

func decodePersistedCodeSpans(t *testing.T, blob json.RawMessage) PersistedCodeSpans {
	t.Helper()
	if len(blob) == 0 {
		t.Fatal("expected a persisted code spans blob")
	}
	var spans PersistedCodeSpans
	if err := json.Unmarshal(blob, &spans); err != nil {
		t.Fatalf("unmarshal persisted code spans: %v", err)
	}
	return spans
}

func TestBuildPersistedCodeSpans(t *testing.T) {
	a := &App{}
	text := "prose\n```python\ndef f():\n    pass\n```\nmore\n```go\nok := true\n```"
	spans := decodePersistedCodeSpans(t, a.buildPersistedCodeSpans(text))

	if spans.Version != highlight.SchemaVersion() {
		t.Fatalf("version = %q, want %q", spans.Version, highlight.SchemaVersion())
	}
	if len(spans.Blocks) != 2 {
		t.Fatalf("blocks = %#v, want 2", spans.Blocks)
	}
	if spans.Blocks[0].Lang != "python" ||
		spans.Blocks[0].ContentKey != highlight.FrontendContentKey("def f():\n    pass") {
		t.Fatalf("python block key wrong: %#v", spans.Blocks[0])
	}
	if len(spans.Blocks[0].Lines) != 2 {
		t.Fatalf("python block lines = %d, want 2", len(spans.Blocks[0].Lines))
	}
	if spans.Blocks[1].Lang != "go" {
		t.Fatalf("go block missing: %#v", spans.Blocks[1])
	}
}

func TestBuildPersistedCodeSpansIncludesTrailingOpenFence(t *testing.T) {
	// A settled summary is final content: an unclosed trailing fence
	// (interrupted stream) renders as-is and its spans persist keyed to
	// that exact content.
	a := &App{}
	spans := decodePersistedCodeSpans(t, a.buildPersistedCodeSpans("```python\npass"))
	if len(spans.Blocks) != 1 ||
		spans.Blocks[0].ContentKey != highlight.FrontendContentKey("pass") {
		t.Fatalf("open trailing fence must persist: %#v", spans.Blocks)
	}
}

func TestBuildPersistedCodeSpansGuards(t *testing.T) {
	a := &App{}
	if got := a.buildPersistedCodeSpans(""); got != nil {
		t.Fatalf("empty text must not build, got %s", got)
	}
	if got := a.buildPersistedCodeSpans("no fences here"); got != nil {
		t.Fatalf("fenceless text must not build, got %s", got)
	}
	over := strings.Repeat("x", seedMaxScanBytes+1)
	if got := a.buildPersistedCodeSpans(over); got != nil {
		t.Fatalf("over-cap text must not build, got %s", got)
	}

	// Languageless, oversized, and invalid-UTF-8 fences are skipped
	// while valid siblings still persist.
	big := "```python\n" + strings.Repeat("x = 1\n", seedMaxSourceBytes/6+1) + "```"
	text := "```\nplain\n```\n" + big + "\n```python\nbad = \"\xff\xfe\"\n```\n```go\nok := true\n```"
	spans := decodePersistedCodeSpans(t, a.buildPersistedCodeSpans(text))
	if len(spans.Blocks) != 1 || spans.Blocks[0].Lang != "go" {
		t.Fatalf("expected only the go fence to persist, got %#v", spans.Blocks)
	}
}
