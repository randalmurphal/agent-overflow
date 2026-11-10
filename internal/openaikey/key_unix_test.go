//go:build unix

package openaikey

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testKey = "sk-proj-AbC123_xyz-789"

// keyFile writes contents to a fresh key path with the given modes.
func keyFile(t *testing.T, contents string, dirMode, fileMode fs.FileMode) string {
	t.Helper()
	path := Path(t.TempDir())
	if err := os.Mkdir(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), fileMode); err != nil {
		t.Fatal(err)
	}
	// WriteFile and Mkdir are subject to the umask; set the modes exactly.
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	return path
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestReadReturnsTheKeyAlone(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"bare":              testKey,
		"echoed":            testKey + "\n",
		"pasted with space": "  " + testKey + " \r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, err := Read(keyFile(t, contents, 0o700, 0o600))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(key.Reveal()); got != testKey {
				t.Fatalf("Read = %q, want %q", got, testKey)
			}
			key.Zero()
		})
	}
}

func TestReadPutsLoosenedModesBack(t *testing.T) {
	t.Parallel()
	// An editor that saves by replacing the file leaves it 0644.
	path := keyFile(t, testKey, 0o755, 0o644)
	key, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	key.Zero()
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if got := mode(t, filepath.Dir(path)); got != 0o700 {
		t.Fatalf("folder mode = %o, want 700", got)
	}
}

func TestReadSaysWhyItHasNoKey(t *testing.T) {
	t.Parallel()
	noFolder := Path(t.TempDir())
	noFile := Path(t.TempDir())
	if err := os.Mkdir(filepath.Dir(noFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want error
	}{
		"no folder":       {noFolder, ErrMissing},
		"no file":         {noFile, ErrMissing},
		"empty":           {keyFile(t, "", 0o700, 0o600), ErrEmpty},
		"only whitespace": {keyFile(t, " \n\t\n", 0o700, 0o600), ErrEmpty},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Read(tc.path); !errors.Is(err, tc.want) {
				t.Fatalf("Read = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadRefusesAFileItCannotVouchFor(t *testing.T) {
	t.Parallel()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinked := Path(t.TempDir())
	if err := os.Mkdir(filepath.Dir(symlinked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, symlinked); err != nil {
		t.Fatal(err)
	}

	realFolder := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realFolder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realFolder, FileName), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkedFolder := Path(t.TempDir())
	if err := os.Symlink(realFolder, filepath.Dir(symlinkedFolder)); err != nil {
		t.Fatal(err)
	}

	aFolder := Path(t.TempDir())
	if err := os.MkdirAll(aFolder, 0o700); err != nil {
		t.Fatal(err)
	}

	fifo := Path(t.TempDir())
	if err := os.Mkdir(filepath.Dir(fifo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	secondLine := "sk-second-" + strings.Repeat("q", 8)
	for name, tc := range map[string]struct {
		path   string
		reason string
	}{
		"a symlink":          {symlinked, "it is a symlink"},
		"a symlinked folder": {symlinkedFolder, "its folder is a symlink"},
		"a folder":           {aFolder, "it is not a regular file"},
		"a fifo":             {fifo, "it is not a regular file"},
		"two keys":           {keyFile(t, testKey+"\n"+secondLine+"\n", 0o700, 0o600), "it holds more than one key; leave only the key in it"},
		"too large":          {keyFile(t, strings.Repeat("k", maxFileBytes+1), 0o700, 0o600), "it is larger than an API key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, err := Read(tc.path)
			var refusal *RefusedError
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("Read = %v (key %v), want refusal %q", err, key, tc.reason)
			}
			for _, leaked := range []string{testKey, secondLine} {
				if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), leaked) {
					t.Fatalf("refusal %v names the file's contents", err)
				}
			}
			if status := Inspect(tc.path); status.State != StateRefused || status.Problem != tc.reason {
				t.Fatalf("Inspect = %+v, want refused %q", status, tc.reason)
			}
		})
	}
}

func TestInspectNeverCarriesTheKey(t *testing.T) {
	t.Parallel()
	status := Inspect(keyFile(t, testKey, 0o700, 0o600))
	if status != (Status{State: StatePresent}) {
		t.Fatalf("Inspect = %+v, want present", status)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", status, status), testKey) {
		t.Fatal("status carries the key")
	}
	missing := Inspect(Path(t.TempDir()))
	if missing != (Status{State: StateMissing}) {
		t.Fatalf("Inspect = %+v, want missing", missing)
	}
}

func TestCreateMakesAnOwnerOnlyEmptyFileOnce(t *testing.T) {
	t.Parallel()
	path := Path(filepath.Join(t.TempDir(), "agent-overflow"))
	made, err := Create(path)
	if err != nil || !made {
		t.Fatalf("Create = %v, %v; want made", made, err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if got := mode(t, filepath.Dir(path)); got != 0o700 {
		t.Fatalf("folder mode = %o, want 700", got)
	}
	if status := Inspect(path); status.State != StateEmpty {
		t.Fatalf("Inspect = %+v, want empty", status)
	}

	if err := os.WriteFile(path, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	made, err = Create(path)
	if err != nil || made {
		t.Fatalf("second Create = %v, %v; want left alone", made, err)
	}
	key, err := Read(path)
	if err != nil || string(key.Reveal()) != testKey {
		t.Fatalf("Read after a second Create = %v; the key must survive", err)
	}
	key.Zero()
}

func TestCreateRefusesASymlinkedFile(t *testing.T) {
	t.Parallel()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path := Path(t.TempDir())
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	var refusal *RefusedError
	if _, err := Create(path); !errors.As(err, &refusal) {
		t.Fatalf("Create = %v, want a refusal", err)
	}
	if got := mode(t, elsewhere); got != 0o644 {
		t.Fatalf("the symlink's target mode changed to %o", got)
	}
}

// A folder replaced after its check cannot redirect the read: the file is
// opened through the folder's own descriptor.
func TestAFolderSwappedAfterItsCheckDoesNotRedirectTheRead(t *testing.T) {
	t.Parallel()
	path := keyFile(t, testKey, 0o700, 0o600)
	dir := filepath.Dir(path)
	folder, err := openFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := folder.Close(); err != nil {
			t.Error(err)
		}
	})

	other := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, FileName), []byte("sk-planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}

	file, err := openIn(folder, FileName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != testKey {
		t.Fatalf("read %q through the checked folder, want the original key", got)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("a read through the swapped-in symlinked folder succeeded")
	}
}
