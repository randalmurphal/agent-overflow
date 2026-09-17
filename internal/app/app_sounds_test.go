package app

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/soundlib"
)

func newSoundTestApp(t *testing.T) (*App, string) {
	t.Helper()
	configDir := t.TempDir()
	return &App{configDir: configDir}, configDir
}

// canonicalCue builds a conforming cue holding sampleBytes of silence. The
// package under test refuses anything else, so a fixture that assembled
// almost-a-WAV would only ever prove the refusal.
func canonicalCue(sampleBytes int) []byte {
	out := make([]byte, 44+sampleBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], 44100)
	binary.LittleEndian.PutUint32(out[28:32], 88200)
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(sampleBytes))
	return out
}

func TestPutGetDeleteSoundFile(t *testing.T) {
	app, configDir := newSoundTestApp(t)
	var changes int
	app.testEmitHook = func(name string, _ any) {
		if name == eventchan.SoundChanged.String() {
			changes++
		}
	}
	wav := canonicalCue(4410)

	if err := app.PutSoundFile("desk-bell", base64.StdEncoding.EncodeToString(wav)); err != nil {
		t.Fatalf("PutSoundFile: %v", err)
	}
	files, err := app.GetSoundFiles()
	if err != nil {
		t.Fatalf("GetSoundFiles: %v", err)
	}
	if files.Dir != filepath.Join(configDir, soundlib.DirName) {
		t.Fatalf("dir = %q, want the sounds subdirectory of %q", files.Dir, configDir)
	}
	if len(files.Sounds) != 1 || files.Sounds[0].ID != "desk-bell" {
		t.Fatalf("sounds = %+v, want the cue that was just written", files.Sounds)
	}
	decoded, err := base64.StdEncoding.DecodeString(files.Sounds[0].WAV)
	if err != nil {
		t.Fatalf("wav is not base64: %v", err)
	}
	if string(decoded) != string(wav) {
		t.Fatal("the listing carried different bytes than were written")
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", files.Warnings)
	}

	if err := app.DeleteSoundFile("desk-bell"); err != nil {
		t.Fatalf("DeleteSoundFile: %v", err)
	}
	if files, err = app.GetSoundFiles(); err != nil {
		t.Fatalf("GetSoundFiles after delete: %v", err)
	}
	if len(files.Sounds) != 0 {
		t.Fatalf("sounds = %+v, want none after the delete", files.Sounds)
	}
	// The watcher's own events for these two writes are suppressed, so the
	// RPCs are the only thing that can tell the OTHER screens attached to
	// this backend that the library changed.
	if changes != 2 {
		t.Fatalf("sound:changed frames = %d, want one per write", changes)
	}
}

func TestPutSoundFileRefusals(t *testing.T) {
	legal := base64.StdEncoding.EncodeToString(canonicalCue(4410))
	for _, tc := range []struct {
		name string
		id   string
		body string
		want string
	}{
		{"an id that is not one", "Desk Bell", legal, "not a valid sound id"},
		{"a traversal attempt", "../../evil", legal, "not a valid sound id"},
		{"a body that is not base64", "desk-bell", "!!! not base64 !!!", "not valid base64"},
		{
			// Checked on the ENCODED length, before the decode allocates
			// three quarters of it.
			name: "a body past the size cap",
			id:   "desk-bell",
			body: strings.Repeat("A", soundlib.MaxWAVBase64Len+4),
			want: "larger than",
		},
		{
			"a body that decodes to something that is not a cue", "desk-bell",
			base64.StdEncoding.EncodeToString([]byte("an mp3, honestly")), "not a canonical cue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, configDir := newSoundTestApp(t)
			err := app.PutSoundFile(tc.id, tc.body)
			if err == nil {
				t.Fatalf("PutSoundFile accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PutSoundFile(%s) = %q, want it to contain %q", tc.name, err, tc.want)
			}
			entries, readErr := os.ReadDir(filepath.Join(configDir, soundlib.DirName))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read sounds dir: %v", readErr)
			}
			for _, entry := range entries {
				if strings.EqualFold(filepath.Ext(entry.Name()), soundlib.FileExt) {
					t.Fatalf("a refused PutSoundFile left %s behind", entry.Name())
				}
			}
		})
	}
}

func TestDeleteSoundFileRefusesAnIDThatIsNotOne(t *testing.T) {
	app, _ := newSoundTestApp(t)
	if err := app.DeleteSoundFile("../../etc/passwd"); err == nil {
		t.Fatal("DeleteSoundFile accepted an id that is not one")
	}
	// A cue that is not there is not an error: two settings screens deleting
	// the same row must not fail the second one.
	if err := app.DeleteSoundFile("never-existed"); err != nil {
		t.Fatalf("DeleteSoundFile (missing) = %v, want nil", err)
	}
}

// A file dropped in by hand is the expected way this directory grows, so the
// listing has to explain a bad one rather than silently skipping it.
func TestGetSoundFilesWarnsAboutAFileThatIsNotACue(t *testing.T) {
	app, configDir := newSoundTestApp(t)
	app.initSoundDirectory()
	t.Cleanup(func() {
		if app.soundWatcher != nil {
			_ = app.soundWatcher.Close()
		}
	})
	dir := filepath.Join(configDir, soundlib.DirName)
	if err := os.WriteFile(filepath.Join(dir, "bogus.wav"), []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := app.GetSoundFiles()
	if err != nil {
		t.Fatalf("GetSoundFiles: %v", err)
	}
	if len(files.Sounds) != 0 {
		t.Fatalf("sounds = %+v, want none", files.Sounds)
	}
	if len(files.Warnings) != 1 || !strings.Contains(files.Warnings[0], "bogus.wav") {
		t.Fatalf("warnings = %v, want exactly one naming the file", files.Warnings)
	}
}

// initSoundDirectory is boot: it materializes the directory, seeds the
// reference, and arms live reload. None of it may fail boot.
func TestInitSoundDirectorySeedsTheReferenceAndArmsTheWatcher(t *testing.T) {
	app, configDir := newSoundTestApp(t)
	app.initSoundDirectory()
	t.Cleanup(func() {
		if app.soundWatcher != nil {
			_ = app.soundWatcher.Close()
		}
	})

	dir := filepath.Join(configDir, soundlib.DirName)
	if _, err := os.Stat(filepath.Join(dir, soundlib.ReferenceFileName)); err != nil {
		t.Fatalf("%s was not seeded: %v", soundlib.ReferenceFileName, err)
	}
	if app.soundWatcher == nil {
		t.Fatal("the sounds watcher did not start")
	}
}

// A blocked sounds directory is degraded, not broken: boot logs and moves on,
// the read RPC still answers, the write RPC reports a real failure, and the
// seed heals from the next read once the blocker is gone.
func TestInitSoundDirectorySurvivesABlockedBoot(t *testing.T) {
	app, configDir := newSoundTestApp(t)
	blocker := filepath.Join(configDir, soundlib.DirName)
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	app.initSoundDirectory()
	t.Cleanup(func() {
		if app.soundWatcher != nil {
			_ = app.soundWatcher.Close()
		}
	})
	if app.soundWatcher != nil {
		t.Fatal("a watcher armed over a path that is a file")
	}

	files, err := app.GetSoundFiles()
	if err != nil {
		t.Fatalf("GetSoundFiles while blocked: %v", err)
	}
	if len(files.Sounds) != 0 {
		t.Fatalf("blocked listing = %+v, want empty", files.Sounds)
	}
	if err := app.PutSoundFile("desk-bell", base64.StdEncoding.EncodeToString(canonicalCue(4410))); err == nil {
		t.Fatal("PutSoundFile succeeded with a file sitting on the directory path")
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if files, err = app.GetSoundFiles(); err != nil {
		t.Fatalf("GetSoundFiles after unblocking: %v", err)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("a healed listing still warns: %v", files.Warnings)
	}
	if _, err := os.Stat(filepath.Join(blocker, soundlib.ReferenceFileName)); err != nil {
		t.Fatalf("the reference was not seeded by the retry: %v", err)
	}
}
