package openaikey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/keyfile"
)

const testKey = "sk-proj-AbC123_xyz-789"

// keyFile makes the key file under a fresh app root through Create and
// writes contents to it, as a user pasting a key would.
func keyFile(t *testing.T, contents string) string {
	t.Helper()
	path := Path(t.TempDir())
	if _, err := Create(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPathIsTheKeyFileUnderTheAppRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if got, want := Path(root), filepath.Join(root, "credentials", "openai-api-key"); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestReadReturnsTheKey(t *testing.T) {
	t.Parallel()
	key, err := Read(keyFile(t, "  "+testKey+" \r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(key.Reveal()); got != testKey {
		t.Fatalf("Read = %q, want %q", got, testKey)
	}
	key.Zero()
}

// Messages name the OpenAI API key file, and errors match both this
// package's sentinels and keyfile's.
func TestMessagesNameTheOpenAIKeyFile(t *testing.T) {
	t.Parallel()
	if ErrMissing.Error() != "there is no OpenAI API key file" || ErrEmpty.Error() != "the OpenAI API key file is empty" {
		t.Fatalf("sentinels = %q, %q", ErrMissing, ErrEmpty)
	}
	_, err := Read(Path(t.TempDir()))
	if !errors.Is(err, ErrMissing) || !errors.Is(err, keyfile.ErrMissing) || err.Error() != "there is no OpenAI API key file" {
		t.Fatalf("Read without a file = %v", err)
	}
	_, err = Read(keyFile(t, " \n\t\n"))
	if !errors.Is(err, ErrEmpty) || !errors.Is(err, keyfile.ErrEmpty) || err.Error() != "the OpenAI API key file is empty" {
		t.Fatalf("Read of an empty file = %v", err)
	}

	secondLine := "sk-second-" + strings.Repeat("q", 8)
	for name, tc := range map[string]struct {
		contents, reason string
	}{
		"two keys":  {testKey + "\n" + secondLine + "\n", "it holds more than one key; leave only the key in it"},
		"too large": {strings.Repeat("k", 4097), "it is larger than an API key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := keyFile(t, tc.contents)
			_, err := Read(path)
			var refusal *RefusedError
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("Read = %v, want refusal %q", err, tc.reason)
			}
			if want := "Agent Overflow will not use the OpenAI API key file: " + tc.reason; err.Error() != want {
				t.Fatalf("Read = %q, want %q", err, want)
			}
			if status := Inspect(path); status != (Status{State: StateRefused, Problem: tc.reason}) {
				t.Fatalf("Inspect = %+v, want refused %q", status, tc.reason)
			}
		})
	}
}

func TestInspectReportsEachState(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		path  string
		state State
		value string
	}{
		"present": {keyFile(t, testKey), StatePresent, "present"},
		"missing": {Path(t.TempDir()), StateMissing, "missing"},
		"empty":   {keyFile(t, ""), StateEmpty, "empty"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if status := Inspect(tc.path); status != (Status{State: tc.state}) || string(status.State) != tc.value {
				t.Fatalf("Inspect = %+v, want %q", status, tc.value)
			}
		})
	}
	if StateRefused != "refused" {
		t.Fatalf("StateRefused = %q", StateRefused)
	}
}

func TestCreateMakesTheFileOnce(t *testing.T) {
	t.Parallel()
	path := Path(filepath.Join(t.TempDir(), "agent-overflow"))
	made, err := Create(path)
	if err != nil || !made {
		t.Fatalf("Create = %v, %v; want made", made, err)
	}
	if status := Inspect(path); status.State != StateEmpty {
		t.Fatalf("Inspect = %+v, want empty", status)
	}
	made, err = Create(path)
	if err != nil || made {
		t.Fatalf("second Create = %v, %v; want left alone", made, err)
	}
}
