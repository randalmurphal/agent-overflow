package usermessage

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

const (
	goShellInvocationVectorsPath = "testdata/shell_invocation_vectors.json"
	tsShellInvocationVectorsPath = "../../frontend/src/test/fixtures/shellInvocationVectors.json"
)

type shellInvocationVectors struct {
	Separator string `json:"separator"`
	MetaBytes int    `json:"metaBytes"`
	Cases     []struct {
		Name     string `json:"name"`
		Text     string `json:"text"`
		Commands []struct {
			Command string `json:"command"`
			Meta    struct {
				Command       string `json:"command"`
				CommandLength int    `json:"commandLength"`
			} `json:"meta"`
		} `json:"commands"`
		Runs    []*[2]int `json:"runs"`
		Without string    `json:"without"`
	} `json:"cases"`
}

// The frontend suite cannot read out of internal/, so the vectors exist
// twice and this keeps them one file.
func TestShellInvocationVectorsAreSharedByteForByte(t *testing.T) {
	goCopy, err := os.ReadFile(goShellInvocationVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	tsCopy, err := os.ReadFile(tsShellInvocationVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(goCopy) != string(tsCopy) {
		t.Fatalf("%s and %s differ; they must be byte-identical", goShellInvocationVectorsPath, tsShellInvocationVectorsPath)
	}
}

// The vectors state what the frontend is given (a row's bounded command and
// its whole length) beside what this side matches (the whole command), so
// one file pins the rule and the projection both sides depend on.
func TestShellInvocationRunsMatchTheSharedVectors(t *testing.T) {
	raw, err := os.ReadFile(goShellInvocationVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var vectors shellInvocationVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Separator != JoinSeparator || vectors.MetaBytes != ShellCommandMetaBytes {
		t.Fatalf("vectors were written for separator %q and %d metadata bytes", vectors.Separator, vectors.MetaBytes)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range vectors.Cases {
		commands := make([]string, 0, len(tc.Commands))
		for _, command := range tc.Commands {
			commands = append(commands, command.Command)
			meta := shellMeta(store.Attachment{Shell: store.AttachmentShell{Command: command.Command}})
			if meta.Command != command.Meta.Command || meta.CommandLength != command.Meta.CommandLength {
				t.Errorf("%s: row metadata of %.20q = %d bytes, length %d; vectors say %d bytes, length %d", tc.Name,
					command.Command, len(meta.Command), meta.CommandLength, len(command.Meta.Command), command.Meta.CommandLength)
			}
		}
		want := make([]PartRun, 0, len(tc.Runs))
		for _, run := range tc.Runs {
			if run == nil {
				want = append(want, PartRun{First: -1, Last: -1})
				continue
			}
			want = append(want, PartRun{First: run[0], Last: run[1]})
		}
		parts := strings.Split(tc.Text, JoinSeparator)
		if got := ShellInvocationRuns(parts, commands); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: runs = %v, want %v", tc.Name, got, want)
		}
		whole := len(want) > 0 && slices.Contains(want, PartRun{First: 0, Last: len(parts) - 1})
		if got := IsShellInvocation(tc.Text, commands); got != whole {
			t.Errorf("%s: IsShellInvocation = %v, want %v", tc.Name, got, whole)
		}
	}
}

func TestUTF16LengthCountsWhatTheFrontendCounts(t *testing.T) {
	for text, want := range map[string]int{
		"": 0, "ls": 2, "é": 1, "😀": 2, "a😀b": 4, "\xff": 1,
	} {
		if got := utf16Length(text); got != want {
			t.Errorf("utf16Length(%q) = %d, want %d", text, got, want)
		}
	}
}
