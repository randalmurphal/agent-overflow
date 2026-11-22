package browser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openDirByName is a profile's OpenDownloadDir for a download directory no
// Manager holds: dir, made if it is missing, opened by its name.
func openDirByName(dir string) func() (*os.Root, error) {
	return func() (*os.Root, error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		return os.OpenRoot(dir)
	}
}

// openRoomIn opens a download room in dir opened by its name
// (openDirByName).
func openRoomIn(dir string, logf func(string, ...any)) (*downloadRoom, error) {
	parent, err := openDirByName(dir)()
	if err != nil {
		return nil, err
	}
	return openDownloadRoom(parent, dir, logf)
}

func newTestRoom(t *testing.T) (*downloadRoom, string) {
	t.Helper()
	dir := t.TempDir()
	room, err := openRoomIn(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(room.close)
	if filepath.Dir(room.path()) != dir {
		t.Fatalf("the room %s is not in the download directory %s", room.path(), dir)
	}
	return room, dir
}

func writeRoomFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is still there: %v", path, err)
	}
}

func wantThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

// Each browser process writes into a room of its own; a finished download
// is handed over to the name the Manager gives it in the profile's
// download directory, and a discarded one loses its partial and its final
// file.
func TestDownloadRoomHandsOverAndDiscards(t *testing.T) {
	room, dir := newTestRoom(t)
	other, err := openRoomIn(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	if other.path() == room.path() {
		t.Fatal("two processes share a download room")
	}
	writeRoomFile(t, filepath.Join(room.path(), "done"))
	path, err := room.handOver("done")
	if err != nil {
		t.Fatalf("hand over: %v", err)
	}
	if want := filepath.Join(dir, "done"); path != want {
		t.Fatalf("handed over to %s, want %s", path, want)
	}
	wantThere(t, path)
	wantGone(t, filepath.Join(room.path(), "done"))

	partial, final := filepath.Join(room.path(), "dropped"+chromiumPartialSuffix), filepath.Join(room.path(), "dropped")
	writeRoomFile(t, partial)
	writeRoomFile(t, final)
	room.discard("dropped")
	wantGone(t, partial)
	wantGone(t, final)

	for _, id := range []downloadID{"", ".", "..", "a/b", "../done"} {
		if _, err := room.handOver(id); err == nil {
			t.Errorf("handed over download %q, which is no file name", id)
		}
	}
	wantThere(t, path)
}

// Once the process has exited, everything in its room goes, a file no
// event named included, apart from the files of the ends still being
// reported, which go with the room at its close; the handed-over files
// and other processes' rooms stay.
func TestDownloadRoomEmptiesAndCloses(t *testing.T) {
	room, dir := newTestRoom(t)
	other, err := openRoomIn(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	writeRoomFile(t, filepath.Join(other.path(), "theirs"))
	writeRoomFile(t, filepath.Join(room.path(), "handed"))
	if _, err := room.handOver("handed"); err != nil {
		t.Fatal(err)
	}
	ending, unknown := filepath.Join(room.path(), "ending"), filepath.Join(room.path(), "never-begun"+chromiumPartialSuffix)
	writeRoomFile(t, ending)
	writeRoomFile(t, unknown)
	if err := os.Mkdir(filepath.Join(room.path(), "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRoomFile(t, filepath.Join(room.path(), "nested", "file"))

	room.empty(map[downloadID]bool{"ending": true})
	wantThere(t, ending)
	wantGone(t, unknown)
	wantGone(t, filepath.Join(room.path(), "nested"))

	room.close()
	wantGone(t, room.path())
	wantThere(t, filepath.Join(dir, "handed"))
	wantThere(t, filepath.Join(other.path(), "theirs"))
	// What runs after the close finds nothing to do.
	room.discard("ending")
	room.empty(nil)
	room.close()
	if _, err := room.handOver("ending"); err == nil {
		t.Fatal("handed a download over from a closed room")
	}
}

// A browser that exits with a download whose start its tracker never
// processed, a start still queued on the ordered input when the
// connection ended, leaves nothing behind: its files are in the
// process's room, which the stop removes.
func TestTheBrowsersStopRemovesADownloadNoEventNamed(t *testing.T) {
	room, dir := newTestRoom(t)
	tracker := newDownloadTracker(engineEvents{}, func(downloadID) {}, room)
	partial := filepath.Join(room.path(), "queued"+chromiumPartialSuffix)
	writeRoomFile(t, partial)
	tracker.stop()
	wantGone(t, partial)
	wantGone(t, room.path())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("%s is left in the download directory", entry.Name())
	}
}

func wantBytes(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s reads %q, %v; want %q", path, got, err, want)
	}
}

// swapRoom moves the room's directory aside and puts another at its name,
// holding file as someone else's; it answers where the room went.
func swapRoom(t *testing.T, room *downloadRoom, file string) string {
	t.Helper()
	aside := room.path() + "-aside"
	if err := os.Rename(room.path(), aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(room.path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(room.path(), file), []byte("not AO's"), 0o600); err != nil {
		t.Fatal(err)
	}
	return aside
}

// A hand-over never replaces a file at the Manager's name for the download.
func TestDownloadRoomHandOverNeverReplacesAFile(t *testing.T) {
	room, dir := newTestRoom(t)
	if err := os.WriteFile(filepath.Join(dir, "G"), []byte("not AO's"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRoomFile(t, filepath.Join(room.path(), "G"))
	if _, err := room.handOver("G"); err == nil {
		t.Fatal("handed over onto a file already there")
	}
	wantBytes(t, filepath.Join(dir, "G"), "not AO's")
}

// A hand-over places only the file the room holds: a directory put at the
// room's name meanwhile hands nothing over, and the room's close removes
// nothing of it.
func TestDownloadRoomActsOnlyOnTheDirectoryItHolds(t *testing.T) {
	room, dir := newTestRoom(t)
	writeRoomFile(t, filepath.Join(room.path(), "G"))
	aside := swapRoom(t, room, "G")
	if _, err := room.handOver("G"); err == nil {
		t.Fatal("handed over from a directory put at the room's name")
	}
	wantGone(t, filepath.Join(dir, "G"))
	room.close()
	wantBytes(t, filepath.Join(room.path(), "G"), "not AO's")
	// The room itself was emptied through its handle; its directory, no
	// longer at its name, stays.
	entries, err := os.ReadDir(aside)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("the room kept %s", entry.Name())
	}
}

// withdraw removes a handed-over file only while its name still holds the
// file handOver placed.
func TestDownloadRoomWithdrawsOnlyWhatItPlaced(t *testing.T) {
	room, dir := newTestRoom(t)
	writeRoomFile(t, filepath.Join(room.path(), "taken"))
	if _, err := room.handOver("taken"); err != nil {
		t.Fatal(err)
	}
	room.withdraw("taken")
	wantGone(t, filepath.Join(dir, "taken"))

	writeRoomFile(t, filepath.Join(room.path(), "replaced"))
	path, err := room.handOver("replaced")
	if err != nil {
		t.Fatal(err)
	}
	// The placed file is moved on, so its identity is not reused.
	if err := os.Rename(path, path+"-taken"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not AO's"), 0o600); err != nil {
		t.Fatal(err)
	}
	room.withdraw("replaced")
	wantBytes(t, path, "not AO's")
}

// A room is made in the download directory held, and Chromium is told its
// path by name: when that name no longer reaches the room held, the
// directory having moved since it was held, the launch is refused, the
// room made is removed from the directory held, and nothing is made at the
// name's new place.
func TestADownloadRoomWhosePathNoLongerReachesItIsRefused(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "downloads")
	parent, err := openDirByName(dir)()
	if err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(base, "held")
	if err := os.Rename(dir, held); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	room, err := openDownloadRoom(parent, dir, t.Logf)
	if err == nil {
		room.close()
		t.Fatalf("a room at %s was opened though that path no longer reaches it", room.path())
	}
	if !strings.Contains(err.Error(), "AO wrote there") {
		t.Fatalf("the launch was refused with %v, want the path's reason", err)
	}
	for _, place := range []string{held, elsewhere} {
		if entries, err := os.ReadDir(place); err != nil || len(entries) != 0 {
			t.Fatalf("%s holds %v after the refused room: %v", place, entries, err)
		}
	}
}
