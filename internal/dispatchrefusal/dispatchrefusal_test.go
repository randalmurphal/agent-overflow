package dispatchrefusal

import (
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

func TestOnlyTheDispatchersOwnRefusalIsWorded(t *testing.T) {
	for _, tc := range []struct {
		code     string
		detailed bool
		want     string
	}{
		{CodeMethodNotFound, false, unsupported},
		{CodeBadParams, false, unreadable},
		{CodeMethodNotFound, true, "wire"},
		{CodeBadParams, true, "wire"},
		{"method_error", false, "wire"},
	} {
		if got := Message(tc.code, "wire", tc.detailed); got != tc.want {
			t.Fatalf("Message(%s, detailed %v) = %q, want %q", tc.code, tc.detailed, got, tc.want)
		}
	}
}
