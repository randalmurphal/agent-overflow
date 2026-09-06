package git

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestCommandStreamsWithoutUsingTheDiagnosticBuffer(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	data := bytes.Repeat([]byte("native and workspace objects\x00"), 32_000)
	hash, _, err := core.executeSpec(commandSpec{binary: "git", cwd: repo, args: []string{"hash-object", "-w", "--stdin"}, input: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	result, err := core.runSpec(commandSpec{binary: "git", cwd: repo, args: []string{"cat-file", "blob", strings.TrimSpace(hash)}, maxBytes: 64, output: &output, outputLimit: int64(len(data))})
	if err != nil || result.exitCode != 0 || result.stdout != "" || !bytes.Equal(data, output.Bytes()) {
		t.Fatalf("stream result: %+v %v, bytes %d", result, err, output.Len())
	}
	output.Reset()
	_, err = core.runSpec(commandSpec{binary: "git", cwd: repo, args: []string{"cat-file", "blob", strings.TrimSpace(hash)}, output: &output, outputLimit: 1024})
	if err == nil || !strings.Contains(err.Error(), "size limit") || output.Len() > 1024 {
		t.Fatalf("unbounded stream: %v bytes %d", err, output.Len())
	}
	_, err = core.runSpec(commandSpec{binary: "git", cwd: repo, args: []string{"cat-file", "blob", strings.TrimSpace(hash)}, output: failingStream{}, outputLimit: int64(len(data))})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost destination write error: %v", err)
	}
}

type failingStream struct{}

func (failingStream) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCommandRefusesAmbiguousOrUnboundedStreams(t *testing.T) {
	for _, spec := range []commandSpec{{stdin: "input", input: strings.NewReader("other")}, {output: io.Discard}} {
		if _, err := NewCore().runSpec(spec); err == nil {
			t.Fatal("invalid streaming command reached the child")
		}
	}
}
