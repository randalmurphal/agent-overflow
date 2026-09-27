package highlightapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPersistedCodeSpansGuardsAndOpenFence(t *testing.T) {
	service := New(Config{})
	blob := service.BuildPersistedCodeSpans("```go\nx := 1")
	var spans PersistedCodeSpans
	if err := json.Unmarshal(blob, &spans); err != nil {
		t.Fatal(err)
	}
	if len(spans.Blocks) != 1 || spans.Blocks[0].ContentKey == "" {
		t.Fatalf("spans = %+v", spans)
	}
	for _, text := range []string{"", "no fences", strings.Repeat("x", codeSpansMaxScanBytes+1)} {
		if got := service.BuildPersistedCodeSpans(text); got != nil {
			t.Fatalf("BuildPersistedCodeSpans(%d bytes) = %s", len(text), got)
		}
	}
}
