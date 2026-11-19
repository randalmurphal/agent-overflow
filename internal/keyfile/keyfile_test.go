package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeyFile makes the file with contents through Create, as a user
// pasting into it would.
func writeKeyFile(t *testing.T, file File, contents string) {
	t.Helper()
	if _, err := file.Create(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.Path(), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every message names the file by its own label, and errors.Is tells the
// labels apart while the package sentinels match them all.
func TestMessagesFollowTheLabel(t *testing.T) {
	t.Parallel()
	key := Label{Service: "Example", Kind: APIKey}
	token := Label{Service: "Example Tracker", Kind: APIToken}
	for _, tc := range []struct {
		label                                Label
		missing, empty, twoSecrets, tooLarge string
		refusedPrefix                        string
	}{
		{
			label:         key,
			missing:       "there is no Example API key file",
			empty:         "the Example API key file is empty",
			twoSecrets:    "it holds more than one key; leave only the key in it",
			tooLarge:      "it is larger than an API key",
			refusedPrefix: "Agent Overflow will not use the Example API key file: ",
		},
		{
			label:         token,
			missing:       "there is no Example Tracker API token file",
			empty:         "the Example Tracker API token file is empty",
			twoSecrets:    "it holds more than one token; leave only the token in it",
			tooLarge:      "it is larger than an API token",
			refusedPrefix: "Agent Overflow will not use the Example Tracker API token file: ",
		},
	} {
		t.Run(tc.label.Service, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			file := func(name string) File { return New(filepath.Join(root, DirName, name), tc.label) }
			other := token
			if tc.label == token {
				other = key
			}

			_, err := file("missing").Read()
			if err == nil || err.Error() != tc.missing {
				t.Fatalf("missing Read = %v, want %q", err, tc.missing)
			}
			if !errors.Is(err, ErrMissing) || !errors.Is(err, tc.label.ErrMissing()) || errors.Is(err, other.ErrMissing()) || errors.Is(err, ErrEmpty) {
				t.Fatalf("missing Read = %v matches the wrong sentinels", err)
			}
			if tc.label.ErrMissing().Error() != tc.missing {
				t.Fatalf("ErrMissing() = %q, want %q", tc.label.ErrMissing(), tc.missing)
			}

			empty := file("empty")
			writeKeyFile(t, empty, "\n")
			_, err = empty.Read()
			if err == nil || err.Error() != tc.empty {
				t.Fatalf("empty Read = %v, want %q", err, tc.empty)
			}
			if !errors.Is(err, ErrEmpty) || !errors.Is(err, tc.label.ErrEmpty()) || errors.Is(err, other.ErrEmpty()) || errors.Is(err, ErrMissing) {
				t.Fatalf("empty Read = %v matches the wrong sentinels", err)
			}

			for name, c := range map[string]struct{ contents, reason string }{
				"two":   {"sk-one sk-two", tc.twoSecrets},
				"large": {strings.Repeat("k", maxFileBytes+1), tc.tooLarge},
			} {
				f := file(name)
				writeKeyFile(t, f, c.contents)
				_, err := f.Read()
				var refusal *RefusedError
				if !errors.As(err, &refusal) || refusal.Reason != c.reason || err.Error() != tc.refusedPrefix+c.reason {
					t.Fatalf("%s Read = %v, want %q", name, err, tc.refusedPrefix+c.reason)
				}
			}
		})
	}
}

func TestNewRefusesALabelThatWouldWordMessagesWrongly(t *testing.T) {
	t.Parallel()
	for name, label := range map[string]Label{
		"no label":        {},
		"no service":      {Kind: APIKey},
		"padded service":  {Service: " Example", Kind: APIKey},
		"no kind":         {Service: "Example"},
		"an unknown kind": {Service: "Example", Kind: "password"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("New accepted %+v", label)
				}
			}()
			New("key", label)
		})
	}
}

func TestAFileNotMadeByNewIsRefused(t *testing.T) {
	t.Parallel()
	for name, use := range map[string]func(File){
		"Read":    func(f File) { _, _ = f.Read() },
		"Inspect": func(f File) { _ = f.Inspect() },
		"Create":  func(f File) { _, _ = f.Create() },
		"Remove":  func(f File) { _ = f.Remove() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("%s ran on a File not made by New", name)
				}
			}()
			use(File{})
		})
	}
}
