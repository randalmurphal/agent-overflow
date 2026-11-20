package browser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func newTestRoom(t *testing.T) (*downloadRoom, string) {
	t.Helper()
	dir := t.TempDir()
	room, err := openDownloadRoom(dir, t.Logf)
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
	other, err := openDownloadRoom(dir, t.Logf)
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
	other, err := openDownloadRoom(dir, t.Logf)
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
