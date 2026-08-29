package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harnessclient"
)

// `await` used to pull the whole replay ring and settle on the OLDEST
// match in it. Every invocation is a fresh connection, so nothing carried
// the "a wait consumes its match" rule across one: waiting for
// provider:turn_completed returned a turn that finished ten minutes ago,
// instantly, forever. The default window is what fixes that, so it is
// pinned here rather than left to the flag declaration.
func TestAwaitDefaultsToEventsAfterItSubscribes(t *testing.T) {
	wantHistory, minSeq, err := parseSince(sinceNow, false)
	if err != nil {
		t.Fatalf("parseSince: %v", err)
	}
	if wantHistory || minSeq != 0 {
		t.Fatalf("--since now asked for history (history=%v minSeq=%d)", wantHistory, minSeq)
	}
}

func TestParseSinceWindows(t *testing.T) {
	for _, tc := range []struct {
		name        string
		since       string
		history     bool
		wantHistory bool
		wantSeq     uint64
		wantErr     bool
	}{
		{name: "empty is now", since: "", wantHistory: false},
		{name: "explicit now", since: sinceNow, wantHistory: false},
		{name: "history flag", since: sinceNow, history: true, wantHistory: true},
		// A seq floor is a statement ABOUT the ring, so the ring is pulled:
		// "after seq 40" while refusing history could only ever match live
		// traffic, which is what --since now already means.
		{name: "seq floor pulls the ring", since: "40", wantHistory: true, wantSeq: 40},
		{name: "seq zero is the whole ring", since: "0", wantHistory: true, wantSeq: 0},
		{name: "garbage", since: "yesterday", wantErr: true},
		{name: "both windows named", since: "40", history: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history, seq, err := parseSince(tc.since, tc.history)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSince(%q, %v) accepted it", tc.since, tc.history)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSince(%q, %v): %v", tc.since, tc.history, err)
			}
			if history != tc.wantHistory || seq != tc.wantSeq {
				t.Fatalf("got history=%v seq=%d, want history=%v seq=%d", history, seq, tc.wantHistory, tc.wantSeq)
			}
		})
	}
}

// One line held the seq, the channel AND the payload, so a fixed-width
// prefix ate the terminal and the only varying half — the reason anyone
// is reading — was what got truncated.
func TestPrintEventPutsThePayloadOnItsOwnLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr, format: "text"}
	if err := e.printEvent(harnessclient.Event{Seq: 12, Channel: "provider:usage", Data: []byte(`{"tokens":9}`)}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two lines, got %q", stdout.String())
	}
	if lines[0] != "12 provider:usage" {
		t.Fatalf("header line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  ") || !strings.Contains(lines[1], `"tokens":9`) {
		t.Fatalf("payload line = %q", lines[1])
	}
}

func TestPrintEventMarksAReplayGap(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr, format: "text"}
	if err := e.printEvent(harnessclient.Event{Seq: 3, Channel: "thread:updated", Gap: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "3 thread:updated [gap]" {
		t.Fatalf("gap marker line = %q", got)
	}
}

func TestEventOutputWriterRefusesBytesOverTheFileBudget(t *testing.T) {
	var out bytes.Buffer
	w := &eventOutputWriter{w: &out, limit: 3}
	if n, err := w.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first write = (%d, %v), want 3 bytes and no error", n, err)
	}
	if n, err := w.Write([]byte("d")); err == nil || n != 0 {
		t.Fatalf("over-budget write = (%d, %v), want refusal without a partial write", n, err)
	}
	if out.String() != "abc" || w.written != 3 {
		t.Fatalf("writer state = (%q, %d), want abc and 3", out.String(), w.written)
	}
}

// An unregistered channel is a WARNING, never a refusal: the harness
// publishes onto caller-named channels through an explicit escape hatch,
// so "not in the registry" means "the backend does not emit this itself".
func TestUnknownChannelsWarnAndRegisteredOnesAreSilent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr, format: "text"}

	e.warnUnknownChannels(stringList{"provider:turn_completed", "harness:mock"})
	if stderr.Len() != 0 {
		t.Fatalf("registered channels warned: %s", stderr.String())
	}

	e.warnUnknownChannels(stringList{"provider:turn_complete"})
	if !strings.Contains(stderr.String(), "provider:turn_complete") {
		t.Fatalf("a typo produced no warning: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "events channels provider") {
		t.Fatalf("the warning does not name the listing that answers it: %q", stderr.String())
	}
}

// channels.go is a hand-kept roll call. Naming the CONSTANTS makes a
// RENAME fail the compile; this is the other half — a channel ADDED to
// internal/eventchan and never listed here, which nothing else would
// catch.
func TestKnownChannelsCoversTheEventChannelRegistry(t *testing.T) {
	declared := parseEventChannelConstants(t, "../../internal/eventchan")
	if len(declared) < 40 {
		t.Fatalf("only %d channel constants parsed; the scan is broken, not the list", len(declared))
	}
	listed := knownChannels()
	for _, channel := range declared {
		if !slices.Contains(listed, channel) {
			t.Errorf("internal/eventchan declares %q and cmd/ao-harness/channels.go does not list it", channel)
		}
	}
	for _, channel := range listed {
		if !slices.Contains(declared, channel) {
			t.Errorf("channels.go lists %q, which internal/eventchan no longer declares", channel)
		}
	}
}

// parseEventChannelConstants reads the wire spellings out of the registry
// package's source. Go cannot enumerate a package's constants at runtime,
// which is the whole reason the CLI keeps a list at all.
func parseEventChannelConstants(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "Channel" {
						continue
					}
					for _, expr := range value.Values {
						literal, ok := expr.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							continue
						}
						names = append(names, strings.Trim(literal.Value, `"`))
					}
				}
			}
		}
	}
	return names
}
