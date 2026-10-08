package app

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"agent-overflow/internal/logging"
)

func TestGetErrorLogLinesReturnsTheLinesUpToTheRef(t *testing.T) {
	t.Parallel()
	app := &App{}
	// Every writer from logging.Output feeds the one retained process log,
	// so this one stands in for the shell's without touching the global
	// logger; a ref unique to this test isolates its lines.
	const ref = "TestGetErrorLogLines-ref"
	out := logging.Output(io.Discard)
	fmt.Fprintln(out, "before the failure")
	fmt.Fprintf(out, "transport: main.App.Thing returned error (id: %s): boom\n", ref)

	got, err := app.GetErrorLogLines(ref)
	if err != nil {
		t.Fatalf("GetErrorLogLines: %v", err)
	}
	if !got.Found || len(got.Lines) == 0 || !strings.Contains(got.Lines[len(got.Lines)-1], "(id: "+ref+")") {
		t.Fatalf("report = %+v, want lines ending at the ref", got)
	}

	missing, err := app.GetErrorLogLines("never-logged")
	if err != nil || missing.Found || missing.Lines == nil || len(missing.Lines) != 0 {
		t.Fatalf("missing ref = %+v err=%v, want found=false with empty lines", missing, err)
	}
	if _, err := app.GetErrorLogLines("  "); err == nil {
		t.Fatal("an empty ref was accepted")
	}
}
