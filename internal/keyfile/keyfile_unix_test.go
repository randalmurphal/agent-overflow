//go:build unix

package keyfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

const testKey = "sk-proj-AbC123_xyz-789"

var testLabel = Label{Service: "Example", Kind: APIKey}

// testFile is a key file under a fresh app root, not yet on disk.
func testFile(t *testing.T) File {
	t.Helper()
	return New(filepath.Join(t.TempDir(), DirName, "example-api-key"), testLabel)
}

// keyFile writes contents to a fresh key file with the given modes.
func keyFile(t *testing.T, contents string, dirMode, fileMode fs.FileMode) File {
	t.Helper()
	file := testFile(t)
	if err := os.Mkdir(filepath.Dir(file.Path()), dirMode); err != nil {
		t.Fatal(err)
	}
	mockexec.WriteFile(t, file.Path(), []byte(contents), fileMode)
	// WriteFile and Mkdir are subject to the umask; set the modes exactly.
	if err := os.Chmod(file.Path(), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(file.Path()), dirMode); err != nil {
		t.Fatal(err)
	}
	return file
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
			key, err := keyFile(t, contents, 0o700, 0o600).Read()
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
	file := keyFile(t, testKey, 0o755, 0o644)
	key, err := file.Read()
	if err != nil {
		t.Fatal(err)
	}
	key.Zero()
	if got := mode(t, file.Path()); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if got := mode(t, filepath.Dir(file.Path())); got != 0o700 {
		t.Fatalf("folder mode = %o, want 700", got)
	}
}

func TestReadSaysWhyItHasNoKey(t *testing.T) {
	t.Parallel()
	noFolder := testFile(t)
	noFile := testFile(t)
	if err := os.Mkdir(filepath.Dir(noFile.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		file File
		want error
	}{
		"no folder":       {noFolder, ErrMissing},
		"no file":         {noFile, ErrMissing},
		"empty":           {keyFile(t, "", 0o700, 0o600), ErrEmpty},
		"only whitespace": {keyFile(t, " \n\t\n", 0o700, 0o600), ErrEmpty},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.file.Read(); !errors.Is(err, tc.want) {
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
	symlinked := testFile(t)
	if err := os.Mkdir(filepath.Dir(symlinked.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, symlinked.Path()); err != nil {
		t.Fatal(err)
	}

	realFolder := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realFolder, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkedFolder := testFile(t)
	if err := os.WriteFile(filepath.Join(realFolder, filepath.Base(symlinkedFolder.Path())), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFolder, filepath.Dir(symlinkedFolder.Path())); err != nil {
		t.Fatal(err)
	}

	aFolder := testFile(t)
	if err := os.MkdirAll(aFolder.Path(), 0o700); err != nil {
		t.Fatal(err)
	}

	folderIsAFile := testFile(t)
	if err := os.WriteFile(filepath.Dir(folderIsAFile.Path()), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}

	fifo := testFile(t)
	if err := os.Mkdir(filepath.Dir(fifo.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo.Path(), 0o600); err != nil {
		t.Fatal(err)
	}

	secondLine := "sk-second-" + strings.Repeat("q", 8)
	for name, tc := range map[string]struct {
		file   File
		reason string
	}{
		"a symlink":            {symlinked, "it is a symlink"},
		"a symlinked folder":   {symlinkedFolder, "its folder is a symlink"},
		"a folder":             {aFolder, "it is not a regular file"},
		"a file as its folder": {folderIsAFile, "its folder is not a folder"},
		"a fifo":               {fifo, "it is not a regular file"},
		"two keys":             {keyFile(t, testKey+"\n"+secondLine+"\n", 0o700, 0o600), "it holds more than one key; leave only the key in it"},
		"a control character":  {keyFile(t, testKey+"\x7f", 0o700, 0o600), "it holds more than one key; leave only the key in it"},
		"too large":            {keyFile(t, strings.Repeat("k", maxFileBytes+1), 0o700, 0o600), "it is larger than an API key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, err := tc.file.Read()
			var refusal *RefusedError
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("Read = %v (key %v), want refusal %q", err, key, tc.reason)
			}
			for _, leaked := range []string{testKey, secondLine} {
				if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), leaked) {
					t.Fatalf("refusal %v names the file's contents", err)
				}
			}
			if status := tc.file.Inspect(); status.State != StateRefused || status.Problem != tc.reason {
				t.Fatalf("Inspect = %+v, want refused %q", status, tc.reason)
			}
		})
	}
}

// The largest file allowed still reads: the bound refuses only past it.
func TestReadTakesAKeyAtTheSizeBound(t *testing.T) {
	t.Parallel()
	padded := testKey + strings.Repeat(" ", maxFileBytes-len(testKey))
	key, err := keyFile(t, padded, 0o700, 0o600).Read()
	if err != nil || string(key.Reveal()) != testKey {
		t.Fatalf("Read at the bound = %v", err)
	}
	key.Zero()
}

// A folder owned by another user is refused, and its mode is left alone.
// The test needs a folder the test's user does not own: the root folder,
// unless the test runs as its owner.
func TestReadRefusesAnotherUsersFolder(t *testing.T) {
	t.Parallel()
	info, err := os.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) == os.Getuid() {
		t.Skip("the test's user owns the root folder")
	}
	file := New("/agent-overflow-no-key", testLabel)
	var refusal *RefusedError
	if _, err := file.Read(); !errors.As(err, &refusal) || refusal.Reason != "another user owns it" {
		t.Fatalf("Read = %v, want refusal %q", err, "another user owns it")
	}
	if err := file.Remove(); !errors.As(err, &refusal) {
		t.Fatalf("Remove = %v, want a refusal", err)
	}
	after, err := os.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != info.Mode() {
		t.Fatalf("the root folder's mode changed from %v to %v", info.Mode(), after.Mode())
	}
}

func TestInspectNeverCarriesTheKey(t *testing.T) {
	t.Parallel()
	status := keyFile(t, testKey, 0o700, 0o600).Inspect()
	if status.State != StatePresent || status.Problem != "" || status.ModifiedAt == 0 {
		t.Fatalf("Inspect = %+v, want present with when it changed", status)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", status, status), testKey) {
		t.Fatal("status carries the key")
	}
	if missing := testFile(t).Inspect(); missing != (Status{State: StateMissing}) {
		t.Fatalf("Inspect = %+v, want missing", missing)
	}
	if empty := keyFile(t, "\n", 0o700, 0o600).Inspect(); empty != (Status{State: StateEmpty}) {
		t.Fatalf("Inspect = %+v, want empty", empty)
	}
}

// Concurrent reads, each repairing loosened modes, all get the key.
func TestConcurrentReadsAllGetTheKey(t *testing.T) {
	t.Parallel()
	file := keyFile(t, testKey+"\n", 0o755, 0o644)
	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Go(func() {
			key, err := file.Read()
			if err != nil {
				errs <- err
				return
			}
			if got := string(key.Reveal()); got != testKey {
				errs <- fmt.Errorf("read %q", got)
			}
			key.Zero()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := mode(t, file.Path()); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
}

func TestCreateMakesAnOwnerOnlyEmptyFileOnce(t *testing.T) {
	t.Parallel()
	file := New(filepath.Join(t.TempDir(), "agent-overflow", DirName, "example-api-key"), testLabel)
	made, err := file.Create()
	if err != nil || !made {
		t.Fatalf("Create = %v, %v; want made", made, err)
	}
	if got := mode(t, file.Path()); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if got := mode(t, filepath.Dir(file.Path())); got != 0o700 {
		t.Fatalf("folder mode = %o, want 700", got)
	}
	if status := file.Inspect(); status.State != StateEmpty {
		t.Fatalf("Inspect = %+v, want empty", status)
	}

	if err := os.WriteFile(file.Path(), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	made, err = file.Create()
	if err != nil || made {
		t.Fatalf("second Create = %v, %v; want left alone", made, err)
	}
	key, err := file.Read()
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
	file := testFile(t)
	if err := os.Mkdir(filepath.Dir(file.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, file.Path()); err != nil {
		t.Fatal(err)
	}
	var refusal *RefusedError
	if _, err := file.Create(); !errors.As(err, &refusal) {
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
	file := keyFile(t, testKey, 0o700, 0o600)
	dir := filepath.Dir(file.Path())
	folder, err := file.openFolder()
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
	if err := os.WriteFile(filepath.Join(other, filepath.Base(file.Path())), []byte("sk-planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}

	opened, err := file.openIn(folder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != testKey {
		t.Fatalf("read %q through the checked folder, want the original key", got)
	}
	if _, err := file.Read(); err == nil {
		t.Fatal("a read through the swapped-in symlinked folder succeeded")
	}
}
