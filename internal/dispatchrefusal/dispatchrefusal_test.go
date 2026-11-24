package dispatchrefusal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sentences, pinned: a change here fails until the frontend's copy
// (and this pin) change with it.
const (
	unsupported = "The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."
	unreadable  = "The computer this reached could not read this request. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."
)

func TestTheSentencesArePinned(t *testing.T) {
	if Unsupported != unsupported || Unreadable != unreadable {
		t.Fatalf("Unsupported = %q\nUnreadable = %q", Unsupported, Unreadable)
	}
	// A release already shipped shows the message as it is, after its
	// userFacingError keeps the text after the last ": ". Every page clamps
	// an RPC error's message to 256 characters (frontend/src/lib/transport/
	// frames.ts clampString), and a computer that forwards the refusal puts
	// the job and the destination before it, so the sentence leaves room.
	for _, sentence := range []string{Unsupported, Unreadable} {
		if strings.Contains(sentence, ":") {
			t.Fatalf("%q holds a colon, which a shipped page would cut at", sentence)
		}
		if len(sentence) > 180 {
			t.Fatalf("%q is %d characters, more than 180", sentence, len(sentence))
		}
	}
}

// The frontend words the same refusals with the same two sentences when
// it has no name for the computer.
func TestTheFrontendSaysTheSameSentences(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "lib", "transport", "dispatchRefusal.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for name, sentence := range map[string]string{"UNSUPPORTED_SENTENCE": unsupported, "UNREADABLE_SENTENCE": unreadable} {
		if !strings.Contains(string(source), name+" =\n  '"+sentence+"'") {
			t.Fatalf("frontend/src/lib/transport/dispatchRefusal.ts %s is not %q", name, sentence)
		}
	}
}

// The frames below are what the releases named sent, before any method ran
// (internal/transport/dispatcher.go at each tag), and what a method sent
// under the same codes.
func TestWhoRefusedIsReadFromTheFieldOrAnOlderReleasesExactText(t *testing.T) {
	count := CountRefusal("GitPush", 1, 2, false, false)
	for _, tc := range []struct {
		name, code, message string
		field               *Refusal
		want                string
	}{
		{"this release, a missing method", CodeMethodNotFound, Unsupported, UnsupportedRefusal(), unsupported},
		{"this release, the argument count", CodeBadParams, Unreadable, count, unreadable + " (GitPush: expected 1 arguments, got 2)"},
		{"this release, an argument that did not decode", CodeBadParams, Unreadable, &Refusal{Kind: KindUnreadable, Method: "ListPRCommits", Param: 2}, unreadable + " (ListPRCommits argument 2 could not be read)"},
		{"v0.0.19, a missing method", CodeMethodNotFound, "method not registered", nil, unsupported},
		{"v0.0.18, the argument count", CodeBadParams, "main.App.GitPush: expected 1 params, got 2", nil, unreadable + " (main.App.GitPush: expected 1 params, got 2)"},
		{"v0.0.17, ListPRCommits' revision", CodeBadParams, "bad parameter 2", nil, unreadable + " (bad parameter 2)"},
		{"v0.0.15, a page of history", CodeBadParams, "bad parameter 3", nil, unreadable + " (bad parameter 3)"},
		{"v0.0.14, too many arguments", CodeBadParams, "too many parameters", nil, unreadable + " (too many parameters)"},
		{"v0.0.19, a variadic method", CodeBadParams, "main.App.Notify: variadic expects at least 1 params, got 0", nil, unreadable + " (main.App.Notify: variadic expects at least 1 params, got 0)"},
		{"a method's own validation", CodeBadParams, "Choose at least one project for the assistant, or all projects.", nil, "Choose at least one project for the assistant, or all projects."},
		{"v0.0.16 forwarding another computer's refusal", CodeMethodNotFound, `Could not read the log of "build" on Nexus: The destination could not complete the request. Its reported error: method not registered. If it is running an older AO build, update it for detailed command errors.`, nil, `Could not read the log of "build" on Nexus: The destination could not complete the request. Its reported error: method not registered. If it is running an older AO build, update it for detailed command errors.`},
		{"another code", "method_error", "method not registered", nil, "method not registered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Message(tc.code, tc.message, tc.field); got != tc.want {
				t.Fatalf("Message = %q, want %q", got, tc.want)
			}
		})
	}
}

// What did not fit comes from a closed set of shapes: a decoder's own text
// can repeat the value it refused, and never reaches the frame.
func TestADecodeRefusalSaysOnlyTypesAndKinds(t *testing.T) {
	var revision string
	mismatch := json.Unmarshal([]byte(`{"sha":"secret-value"}`), &revision)
	if got := DecodeRefusal("ListPRCommits", 2, mismatch); got.Problem != "expected string, got object" || got.Method != "ListPRCommits" || got.Param != 2 {
		t.Fatalf("type mismatch = %+v", got)
	}
	var count int
	number := json.Unmarshal([]byte(`-5.5`), &count)
	if got := DecodeRefusal("X", 1, number); got.Problem != "expected int, got number" {
		t.Fatalf("number = %+v", got)
	}
	var nested struct {
		Rev struct {
			SHA string `json:"sha"`
		} `json:"rev"`
	}
	field := json.Unmarshal([]byte(`{"rev":{"sha":7}}`), &nested)
	if got := DecodeRefusal("X", 1, field); got.Problem != "expected string at rev.sha, got number" {
		t.Fatalf("nested = %+v", got)
	}
	var refuses echoing
	custom := json.Unmarshal([]byte(`"secret-value"`), &refuses)
	got := DecodeRefusal("SetThing", 1, custom)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Problem != "" || strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("a custom decoder's rejection = %s, want no problem text", encoded)
	}
	if got.Diagnostic() != "SetThing argument 1 could not be read" {
		t.Fatalf("diagnostic = %q", got.Diagnostic())
	}
}

// echoing rejects every value, repeating it, as a careless decoder would.
type echoing struct{}

func (*echoing) UnmarshalJSON(raw []byte) error {
	return fmt.Errorf("not a valid thing: %s", raw)
}

// The frontend reads frames without the field from the same table.
func TestTheFrontendHasTheSameLegacyTable(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "lib", "transport", "dispatchRefusal.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range legacy {
		want := fmt.Sprintf("{ code: '%s', kind: '%s', text: /%s/ }", entry.code, entry.kind, entry.text.String())
		if !strings.Contains(string(source), want) {
			t.Fatalf("frontend/src/lib/transport/dispatchRefusal.ts lacks %s", want)
		}
	}
}

// Both sides read every frame shape to the same refusal, or to none: the
// frontend runs the same cases (frontend/src/lib/transport/dispatchRefusal.test.ts).
func TestTheFrontendReadsEveryFrameAsThisPackageDoes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "test", "fixtures", "dispatchRefusalFrames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string   `json:"name"`
		Code     string   `json:"code"`
		Message  string   `json:"message"`
		Dispatch *Refusal `json:"dispatch"`
		Kind     *string  `json:"kind"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		got := Read(tc.Code, tc.Message, tc.Dispatch)
		switch {
		case tc.Kind == nil && got != nil:
			t.Errorf("%s: read as %q, want a method's own failure", tc.Name, got.Kind)
		case tc.Kind != nil && (got == nil || got.Kind != *tc.Kind):
			t.Errorf("%s: read as %+v, want %q", tc.Name, got, *tc.Kind)
		case tc.Kind == nil && Message(tc.Code, tc.Message, tc.Dispatch) != tc.Message:
			t.Errorf("%s: shown as %q, want the message as sent", tc.Name, Message(tc.Code, tc.Message, tc.Dispatch))
		}
	}
}
