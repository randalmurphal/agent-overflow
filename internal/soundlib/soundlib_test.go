package soundlib

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newService(t *testing.T) *Service {
	t.Helper()
	service, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := service.EnsureBoot(); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	return service
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestEnsureBootSeedsTheReference(t *testing.T) {
	service := newService(t)
	path := filepath.Join(service.Dir(), ReferenceFileName)
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", ReferenceFileName, err)
	}
	want, err := assets.ReadFile("assets/" + ReferenceFileName)
	if err != nil {
		t.Fatalf("read embedded reference: %v", err)
	}
	if string(seeded) != string(want) {
		t.Fatal("the seeded reference differs from the embedded one")
	}
	// The reference is GENERATED: a stale on-disk copy would document a
	// format this build refuses, so boot overwrites a local edit.
	writeFile(t, service.Dir(), ReferenceFileName, []byte("stale"))
	if err := service.EnsureBoot(); err != nil {
		t.Fatalf("EnsureBoot (refresh): %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read %s: %v", ReferenceFileName, err)
	}
	if string(again) != string(want) {
		t.Fatal("EnsureBoot did not refresh an out-of-date reference")
	}
}

// A blocked boot heals on the next listing rather than waiting for the next
// launch: the blocker is something the user removes out-of-band.
func TestFilesRetriesABlockedBoot(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, DirName)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	service, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := service.EnsureBoot(); err == nil {
		t.Fatal("EnsureBoot succeeded with a file sitting on the directory path")
	}
	got := service.Files()
	if len(got.Sounds) != 0 {
		t.Fatalf("sounds = %d, want none while the directory is blocked", len(got.Sounds))
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	if got := service.Files(); len(got.Warnings) != 0 {
		t.Fatalf("warnings after the blocker was removed = %v, want none", got.Warnings)
	}
	if _, err := os.Stat(filepath.Join(blocker, ReferenceFileName)); err != nil {
		t.Fatalf("the reference was not seeded by the retry: %v", err)
	}
}

func TestFilesListsValidatedCuesInOrder(t *testing.T) {
	service := newService(t)
	wav := canonicalWAV(wavByteRate / 10)
	for _, id := range []string{"zebra", "alpha", "middle"} {
		writeFile(t, service.Dir(), id+FileExt, wav)
	}
	got := service.Files()
	if len(got.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", got.Warnings)
	}
	var ids []string
	for _, sound := range got.Sounds {
		ids = append(ids, sound.ID)
		decoded, err := base64.StdEncoding.DecodeString(sound.WAV)
		if err != nil {
			t.Fatalf("decode %s: %v", sound.ID, err)
		}
		if string(decoded) != string(wav) {
			t.Fatalf("%s carried different bytes than were written", sound.ID)
		}
	}
	if strings.Join(ids, ",") != "alpha,middle,zebra" {
		t.Fatalf("ids = %v, want them sorted", ids)
	}
	if got.Dir != service.Dir() {
		t.Fatalf("dir = %q, want %q", got.Dir, service.Dir())
	}
}

// The listing is the last gate before a cue reaches a speaker, and the
// directory is editable, so a file that was never written through Put has to
// be caught here.
func TestFilesWarnsAboutEveryUnusableFile(t *testing.T) {
	service := newService(t)
	good := canonicalWAV(wavByteRate / 10)
	writeFile(t, service.Dir(), "good"+FileExt, good)
	writeFile(t, service.Dir(), "bogus"+FileExt, []byte("this is an MP3, really"))
	writeFile(t, service.Dir(), "Not An Id"+FileExt, good)
	writeFile(t, service.Dir(), "toolong"+FileExt, canonicalWAV(MaxSoundDataBytes+wavBlockAlign))
	// A cue is exactly <id>.wav: an upper-case extension is a mistake worth
	// naming, not a cue listed under a file name that does not exist.
	writeFile(t, service.Dir(), "shouty.WAV", good)
	// Neither a cue nor a mistake: other extensions are simply not this
	// directory's business and must not produce noise.
	writeFile(t, service.Dir(), "notes.txt", []byte("hello"))

	got := service.Files()
	if len(got.Sounds) != 1 || got.Sounds[0].ID != "good" {
		t.Fatalf("sounds = %+v, want only the valid cue", got.Sounds)
	}
	joined := strings.Join(got.Warnings, "\n")
	for _, want := range []string{"bogus.wav", "Not An Id.wav", "toolong.wav", "shouty.WAV"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings %v do not name %s", got.Warnings, want)
		}
	}
	if len(got.Warnings) != 4 {
		t.Fatalf("warnings = %v, want exactly one per unusable file", got.Warnings)
	}
}

// Past the count cap the directory is not a cue library any more, and one
// warning explains that better than dozens of identical ones.
func TestFilesStopsAtTheCountCap(t *testing.T) {
	service := newService(t)
	wav := canonicalWAV(wavBlockAlign)
	for i := range MaxSounds + 5 {
		writeFile(t, service.Dir(), string(rune('a'+i/26))+string(rune('a'+i%26))+FileExt, wav)
	}
	got := service.Files()
	if len(got.Sounds) != MaxSounds {
		t.Fatalf("sounds = %d, want the %d cap", len(got.Sounds), MaxSounds)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "stopped after") {
		t.Fatalf("warnings = %v, want one explaining the cap", got.Warnings)
	}
}

// The per-file cap and the count cap are the whole bound on one listing: a
// third check for the aggregate byte total could never run, so the constant
// naming that total has to stay derived from the other two.
func TestListingBoundIsTheCountCap(t *testing.T) {
	if MaxSoundsBytes != MaxSounds*MaxSoundBytes {
		t.Fatalf("MaxSoundsBytes = %d, want %d", MaxSoundsBytes, MaxSounds*MaxSoundBytes)
	}
}

func TestPutWritesOnePrivateFile(t *testing.T) {
	service := newService(t)
	wav := canonicalWAV(wavByteRate / 10)
	if err := service.Put("desk-bell", wav); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(service.Dir(), "desk-bell"+FileExt)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	got := service.Files()
	if len(got.Sounds) != 1 || got.Sounds[0].ID != "desk-bell" {
		t.Fatalf("sounds = %+v, want the cue that was just written", got.Sounds)
	}
}

func TestPutRefusals(t *testing.T) {
	wav := canonicalWAV(wavByteRate / 10)
	for _, tc := range []struct {
		name string
		id   string
		wav  []byte
		want string
	}{
		{"an empty id", "", wav, "not a valid sound id"},
		{"an id with spaces", "desk bell", wav, "not a valid sound id"},
		{"a traversal attempt", "../../evil", wav, "not a valid sound id"},
		{"an uppercase id", "DeskBell", wav, "not a valid sound id"},
		{"a file that is not a canonical cue", "desk-bell", []byte("nope"), "is not a canonical cue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newService(t)
			err := service.Put(tc.id, tc.wav)
			if err == nil {
				t.Fatalf("Put accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Put(%s) = %q, want it to contain %q", tc.name, err, tc.want)
			}
			entries, readErr := os.ReadDir(service.Dir())
			if readErr != nil {
				t.Fatalf("read dir: %v", readErr)
			}
			for _, entry := range entries {
				if entry.Name() != ReferenceFileName {
					t.Fatalf("a refused Put left %s behind", entry.Name())
				}
			}
		})
	}
}

// Overwriting silently would replace the cue three events may already point
// at, using an id derived from a filename the user did not choose carefully.
func TestPutRefusesAnExistingID(t *testing.T) {
	service := newService(t)
	first := canonicalWAV(wavByteRate / 10)
	if err := service.Put("desk-bell", first); err != nil {
		t.Fatalf("Put: %v", err)
	}
	err := service.Put("desk-bell", canonicalWAV(wavByteRate/5))
	if err == nil {
		t.Fatal("Put overwrote an existing cue")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Put = %q, want it to say the id already exists", err)
	}
	kept, readErr := os.ReadFile(filepath.Join(service.Dir(), "desk-bell"+FileExt))
	if readErr != nil {
		t.Fatalf("read kept file: %v", readErr)
	}
	if string(kept) != string(first) {
		t.Fatal("the refused Put changed the existing cue")
	}
}

func TestPutRefusesPastTheCountCap(t *testing.T) {
	service := newService(t)
	wav := canonicalWAV(wavBlockAlign)
	for i := range MaxSounds {
		writeFile(t, service.Dir(), string(rune('a'+i/26))+string(rune('a'+i%26))+FileExt, wav)
	}
	err := service.Put("one-too-many", wav)
	if err == nil {
		t.Fatal("Put accepted a cue past the count cap")
	}
	if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("Put = %q, want it to name the cap", err)
	}
	if _, statErr := os.Stat(filepath.Join(service.Dir(), "one-too-many"+FileExt)); !os.IsNotExist(statErr) {
		t.Fatal("the refused Put wrote a file anyway")
	}
}

func TestDelete(t *testing.T) {
	service := newService(t)
	if err := service.Put("desk-bell", canonicalWAV(wavByteRate/10)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := service.Delete("desk-bell"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(service.Files().Sounds) != 0 {
		t.Fatal("the cue survived Delete")
	}
	// Idempotent: two screens deleting the same row must not fail the
	// second one.
	if err := service.Delete("desk-bell"); err != nil {
		t.Fatalf("Delete (missing) = %v, want nil", err)
	}
	if err := service.Delete("../../etc/passwd"); err == nil {
		t.Fatal("Delete accepted an id that is not one")
	}
}

func TestPathValidatesTheID(t *testing.T) {
	service := newService(t)
	path, err := service.Path("desk-bell")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if path != filepath.Join(service.Dir(), "desk-bell"+FileExt) {
		t.Fatalf("Path = %q", path)
	}
	if _, err := service.Path("../escape"); err == nil {
		t.Fatal("Path accepted an id that is not one")
	}
}

// The id rule crosses into internal/settings and from there into generated
// TypeScript. The compiled matcher and the exported source have to agree, or
// a cue the backend accepts is one the frontend's settings validator drops.
func TestIDPatternSourceMatchesTheCompiledRule(t *testing.T) {
	if idPattern.String() != "^"+IDPattern+"$" {
		t.Fatalf("idPattern = %q, want the anchored form of IDPattern %q", idPattern.String(), IDPattern)
	}
}
