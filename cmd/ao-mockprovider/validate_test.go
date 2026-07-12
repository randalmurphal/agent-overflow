package main

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func newBufReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, 64*1024)
}

func providerDetect(t *testing.T, name string) provider.ProviderStatus {
	t.Helper()
	return provider.DetectProvider(name, mockBin)
}

// validateClaudeFrames feeds captured mock stdout through the real
// Claude parser (the exact code the app's read loop uses) and asserts
// the frames produce a coherent init → text → turn-complete stream.
// control_response lines are excluded the same way session.go's
// prefix-gated read loop consumes them before ParseLine.
func validateClaudeFrames(t *testing.T, lines []string) {
	t.Helper()
	parser := claude.NewParser()
	defer parser.Close()

	kinds := map[provider.EventKind]int{}
	var text strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, `{"type":"control_response"`) ||
			strings.HasPrefix(line, `{"type":"control_request"`) {
			continue
		}
		events, err := parser.ParseLine("thread-1", []byte(line))
		if err != nil {
			t.Fatalf("real Claude parser rejected mock frame: %v\nframe: %s", err, line)
		}
		for _, evt := range events {
			kinds[evt.Kind]++
			if evt.Kind == provider.EventTextDelta {
				text.WriteString(evt.Content)
			}
		}
	}

	if kinds[provider.EventInit] == 0 {
		t.Fatalf("no init event parsed from mock frames; kinds = %v", kinds)
	}
	if kinds[provider.EventTurnComplete] == 0 {
		t.Fatalf("no turn-complete event parsed from mock frames; kinds = %v", kinds)
	}
	if text.Len() == 0 {
		t.Fatalf("no streamed text parsed from mock frames; kinds = %v", kinds)
	}
}
