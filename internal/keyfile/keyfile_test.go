package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// StatusOf gives the state of the read that returned the error, as
// Inspect would for the same file.
func TestStatusOfIsTheStateOfOneRead(t *testing.T) {
	t.Parallel()
	label := Label{Service: "Example", Kind: APIToken}
	root := t.TempDir()
	file := func(name, contents string) File {
		f := New(filepath.Join(root, DirName, name), label)
		if contents != "" {
			writeKeyFile(t, f, contents)
		}
		return f
	}
	// The file's own modification time, which a read leaves as it is.
	saved := time.UnixMilli(1_700_000_000_123)
	present := file("present", "tok-1\n")
	if err := os.Chtimes(present.Path(), saved, saved); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file File
		want Status
	}{
		{present, Status{State: StatePresent, ModifiedAt: saved.UnixMilli()}},
		{file("missing", ""), Status{State: StateMissing}},
		{file("empty", " \n"), Status{State: StateEmpty}},
		{file("two", "tok-1 tok-2"), Status{State: StateRefused, Problem: "it holds more than one token; leave only the token in it"}},
	} {
		key, modified, err := tc.file.ReadModified()
		key.Zero()
		if got := StatusOf(modified, err); got != tc.want || tc.file.Inspect() != tc.want {
			t.Errorf("%s: StatusOf = %+v, Inspect = %+v; want %+v", filepath.Base(tc.file.Path()), got, tc.file.Inspect(), tc.want)
		}
	}
	if info, err := os.Stat(present.Path()); err != nil || !info.ModTime().Equal(saved) {
		t.Errorf("after its reads the file was modified at %v (%v), want %v", info.ModTime(), err, saved)
	}
	other := errors.New("read the Example API token file: input/output error")
	if got := StatusOf(time.Time{}, other); got != (Status{State: StateRefused, Problem: other.Error()}) {
		t.Errorf("StatusOf(another error) = %+v", got)
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
