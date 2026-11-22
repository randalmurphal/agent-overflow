package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The fake CLI appends each stdin line to its log while a test reads it, so
// a read can end inside the fake's write of a line, at a page boundary.
// recordedUserEnvelopes is for a reader that polls until something appears:
// it leaves such a line for its next read. settledUserEnvelopes is for a
// one-shot check that nothing more was sent: a line still being written is
// something sent, so it fails the test.

// recordedUserEnvelopes returns the `type:"user"` lines the fake CLI has
// finished writing. Control requests (permission mode) share the stream and
// are ignored.
func recordedUserEnvelopes(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	envelopes, _, err := parseStdinLog(readStdinLog(t, logPath))
	if err != nil {
		t.Fatal(err)
	}
	return envelopes
}

// settledUserEnvelopes is recordedUserEnvelopes for a log that must hold no
// line in progress.
func settledUserEnvelopes(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	envelopes, err := settledEnvelopes(readStdinLog(t, logPath))
	if err != nil {
		t.Fatal(err)
	}
	return envelopes
}

func readStdinLog(t *testing.T, logPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read stdin log: %v", err)
	}
	return data
}

// settledEnvelopes is parseStdinLog refusing a line in progress.
func settledEnvelopes(data []byte) ([]map[string]any, error) {
	envelopes, partial, err := parseStdinLog(data)
	if err != nil {
		return nil, err
	}
	if len(partial) > 0 {
		return nil, fmt.Errorf("the stdin log ends inside a line the fake is still writing, so something was sent: %q", partial)
	}
	return envelopes, nil
}

// parseStdinLog returns the user envelopes on data's finished lines and the
// bytes after its last newline.
func parseStdinLog(data []byte) ([]map[string]any, []byte, error) {
	cut := bytes.LastIndexByte(data, '\n') + 1
	var out []map[string]any
	for _, line := range strings.Split(string(data[:cut]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return nil, nil, fmt.Errorf("stdin line is not JSON (%v): %s", err, line)
		}
		if kind, _ := envelope["type"].(string); kind == "user" {
			out = append(out, envelope)
		}
	}
	return out, data[cut:], nil
}

// A polling read leaves a line in progress for its next read; a settled read
// fails on it, and on nothing else.
func TestTheStdinLogReadersDisagreeOnlyOnALineInProgress(t *testing.T) {
	t.Parallel()
	user := `{"type":"user","message":{"content":"hi"}}`
	control := `{"type":"control_request","request":{"subtype":"set_permission_mode"}}`
	for _, c := range []struct {
		name, log string
		polled    int
		settled   string
	}{
		{name: "an empty log", log: ""},
		{name: "a control request", log: control + "\n"},
		{name: "one user line", log: user + "\n", polled: 1},
		{name: "a user line in progress", log: control + "\n" + user[:20], settled: "still writing"},
		{name: "a second user line in progress", log: user + "\n" + user[:20], polled: 1, settled: "still writing"},
	} {
		polled, partial, err := parseStdinLog([]byte(c.log))
		if err != nil || len(polled) != c.polled {
			t.Errorf("%s: polled %d envelopes, %v; want %d", c.name, len(polled), err, c.polled)
		}
		settled, err := settledEnvelopes([]byte(c.log))
		switch {
		case c.settled == "" && (err != nil || len(settled) != c.polled):
			t.Errorf("%s: settled %d envelopes, %v; want %d", c.name, len(settled), err, c.polled)
		case c.settled != "" && (err == nil || !strings.Contains(err.Error(), c.settled)):
			t.Errorf("%s: settled read = %v (partial %q), want it refused", c.name, err, partial)
		}
	}
}
