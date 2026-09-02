//go:build !windows

package supervise

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PreflightBinary is the question two processes ask: the supervisor before it
// records a pending update, and the backend before it stages a downloaded
// artifact. The stand-ins here are the same shape the supervisor's own tests
// use — a small script, an isolated home, nothing that reaches a provider —
// so what is proved is the shared implementation rather than a copy of it.

// writePreflightScript stages a script that answers the preflight the way a
// real binary does. body is the whole program, so a test can describe a binary
// that refuses, prints nonsense, or prints nothing at all.
func writePreflightScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "staged-binary")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestPreflightBinaryReadsAStagedBinarysAnswer(t *testing.T) {
	binary := writePreflightScript(t, `printf '{"protocolVersion":1,"version":"1.4.0"}\n'`)

	answer, err := PreflightBinary(context.Background(), binary)
	if err != nil {
		t.Fatalf("PreflightBinary: %v", err)
	}
	if answer.Version != "1.4.0" || answer.ProtocolVersion != 1 {
		t.Fatalf("answer = %+v, want version 1.4.0 protocol 1", answer)
	}
	// The two things the caller does with it: name a version directory, and
	// decide whether this supervisor can talk to what is inside it.
	if err := ValidVersion(answer.Version); err != nil {
		t.Errorf("ValidVersion(%q): %v", answer.Version, err)
	}
	if err := CheckPreflight(answer); err != nil {
		t.Errorf("CheckPreflight: %v", err)
	}
}

// Leading output is tolerated, because a binary may print before it gets to
// its answer. The LAST non-empty line is the answer.
func TestPreflightBinaryTakesTheLastLineAsTheAnswer(t *testing.T) {
	binary := writePreflightScript(t, `printf 'warning: locale not set\n'
printf '{"protocolVersion":1,"version":"2.0.0"}\n'`)

	answer, err := PreflightBinary(context.Background(), binary)
	if err != nil {
		t.Fatalf("PreflightBinary: %v", err)
	}
	if answer.Version != "2.0.0" {
		t.Fatalf("version = %q, want 2.0.0", answer.Version)
	}
}

// A binary that exits non-zero, prints nonsense, or is not there at all is not
// something to stage, and each one has to fail rather than resolve to a zero
// Preflight the caller would then treat as protocol 0.
func TestPreflightBinaryRefusesWhatItCannotRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed")
	for _, tc := range []struct {
		name   string
		binary string
	}{
		{"exits non-zero", writePreflightScript(t, `exit 3`)},
		{"prints nothing", writePreflightScript(t, `exit 0`)},
		{"prints nonsense", writePreflightScript(t, `printf 'not json\n'`)},
		{"names no protocol", writePreflightScript(t, `printf '{"version":"1.0.0"}\n'`)},
		{"is not there", missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if answer, err := PreflightBinary(context.Background(), tc.binary); err == nil {
				t.Fatalf("PreflightBinary answered %+v, want a refusal", answer)
			}
		})
	}
}

// A newer protocol resolves fine and is refused by CheckPreflight, which is
// the split the remote path depends on: the answer is readable, and the
// refusal names the one local command that fixes it.
func TestPreflightBinaryReportsAProtocolThisSupervisorCannotSpeak(t *testing.T) {
	binary := writePreflightScript(t, `printf '{"protocolVersion":99,"version":"9.0.0"}\n'`)

	answer, err := PreflightBinary(context.Background(), binary)
	if err != nil {
		t.Fatalf("PreflightBinary: %v", err)
	}
	err = CheckPreflight(answer)
	if err == nil {
		t.Fatal("CheckPreflight accepted a newer protocol")
	}
	if !strings.Contains(err.Error(), "service update") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
}

// The supervisor and the exported call are ONE implementation. If they ever
// became two, this is the assertion that notices: same binary, same answer,
// from both entry points.
func TestTheSupervisorAsksThroughTheSameImplementation(t *testing.T) {
	binary := writePreflightScript(t, `printf '{"protocolVersion":1,"version":"3.1.4"}\n'`)
	supervisor, err := New(Config{
		DataDir:        t.TempDir(),
		SelfExecutable: binary,
		SelfVersion:    "3.1.4",
		// PATH so the shell resolves, and a HOME that is not the developer's.
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mine, err := PreflightBinary(context.Background(), binary)
	if err != nil {
		t.Fatalf("PreflightBinary: %v", err)
	}
	theirs, err := supervisor.preflight(binary)
	if err != nil {
		t.Fatalf("supervisor.preflight: %v", err)
	}
	if mine != theirs {
		t.Fatalf("the two callers read different answers: %+v vs %+v", mine, theirs)
	}
}
