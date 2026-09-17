package soundlib

import (
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"agent-overflow/internal/atomicfile"
)

// assets holds the reference page seeded into the user's sounds directory
// at boot. The embedded copy is authoritative and the on-disk copy is
// refreshed whenever it differs — it documents THIS build's cue contract,
// so a stale copy would teach an agent a format the app now refuses.
//
//go:embed assets/*
var assets embed.FS

const (
	// DirName is the sounds directory's basename inside <configDir>.
	DirName = "sounds"
	// ReferenceFileName is the generated reference seeded from the
	// embedded assets, named in caps like themes' TOKENS.md and spinners'
	// SPINNERS.md because it is documentation sitting in a directory of
	// content files.
	ReferenceFileName = "SOUNDS.md"
	// FileExt is the only extension this directory holds. A cue is one
	// file, not a pair: the format carries its own geometry.
	FileExt = ".wav"

	// MaxSounds caps the LISTING. Combined with the per-file cap that
	// ValidateWAV makes exact, it is what turns the answer's size into a
	// property of the format instead of a property of whatever the user
	// (or a runaway agent) left in the directory.
	MaxSounds = 32

	// MaxSoundsBytes is the resulting worst case for one listing, stated
	// so the wire cost is readable without multiplying two constants in
	// your head. There is no separate runtime check for it: every file
	// admitted to the listing has already been bounded at MaxSoundBytes,
	// so the count cap alone enforces this, and a second branch for it
	// could never run. TestListingBoundIsTheCountCap pins that.
	MaxSoundsBytes = MaxSounds * MaxSoundBytes
)

// privateDirPerm matches internal/atomicfile's directory mode: cue writes
// go through that package, and the directory it creates on demand must not
// disagree with the one EnsureBoot creates up front.
const privateDirPerm os.FileMode = 0o700

// idPattern is the cue-id shape, identical to the spinner and theme id
// rule: kebab-case ASCII, anchored and length-bounded so an id can never
// escape its directory or become an unwieldy label.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// IDPattern is the same rule as a regular-expression SOURCE, for the one
// place that has to restate it in another language: internal/settings
// builds the `custom:<id>` settings-value pattern from it and generates
// the frontend's copy, so the two cannot drift.
const IDPattern = `[a-z0-9][a-z0-9-]{0,63}`

// Sound is one custom cue as it crosses the wire: its id and the validated
// file, base64-encoded.
//
// WAV is a base64 string rather than a []byte for the mechanical reason
// spinner.Sprite.PNG is: encoding/json base64-encodes a []byte at runtime,
// but the Wails binding generator has no special case for it and would emit
// `number[]` in TypeScript — a declared type that disagrees with the wire.
// A cue is at most 258 KiB, so it belongs in a frame rather than on the
// attachment stream.
type Sound struct {
	ID  string `json:"id"`
	WAV string `json:"wav"`
}

// Files is the sounds listing: where the directory is, every playable cue,
// and every reason a file could not be used.
//
// Warnings are DATA, not log lines. A file that is not a canonical cue is
// the expected failure here — the user dropped an MP3 in, or an encoder
// wrote a LIST chunk — and the symptom without a warning is "my sound
// silently does nothing".
type Files struct {
	Dir      string   `json:"dir"`
	Sounds   []Sound  `json:"sounds"`
	Warnings []string `json:"warnings"`
}

// Service owns <configDir>/sounds and serializes boot and every write
// against it under a private mutex.
type Service struct {
	mu  sync.Mutex
	dir string

	// bootPending records that EnsureBoot ran and the sounds directory
	// still does not exist — something is sitting on the path (a FILE with
	// that name, a read-only parent, a full disk). Files() retries, so the
	// seed heals the moment the blocker is removed rather than waiting for
	// the next launch.
	bootPending bool
}

// New returns a Service rooted at <configDir>/sounds. When configDir is
// empty it falls back to ~/.agent-overflow/sounds so an early-boot RPC
// still resolves a writable path — the same fallback theme.New and
// spinner.New use.
func New(configDir string) (*Service, error) {
	if strings.TrimSpace(configDir) != "" {
		return &Service{dir: filepath.Join(configDir, DirName)}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("soundlib: cannot determine home directory: %w", err)
	}
	return &Service{dir: filepath.Join(home, ".agent-overflow", DirName)}, nil
}

// Dir returns the absolute sounds-directory path.
func (s *Service) Dir() string { return s.dir }

// Path answers where one cue id lives, refusing an id that is not one.
// The App uses it to bracket its own writes with watcher suppression, and
// validating here is what keeps an arbitrary wire string out of that
// suppression ledger.
func (s *Service) Path(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+FileExt), nil
}

// EnsureBoot creates the sounds directory and refreshes the generated
// reference page from the embedded copy.
//
// The reference is refreshed rather than merely seeded because it is
// GENERATED: the embedded copy is what this build's cue contract actually
// is, so a stale on-disk copy would document a format the app no longer
// accepts. Local edits to it are expected to be lost; its first line says
// so.
//
// A failed boot is REMEMBERED rather than lost: see bootPending and
// retryPendingBoot below.
func (s *Service) EnsureBoot() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureBootLocked()
}

// ensureBootLocked is EnsureBoot's body plus the pending-boot bookkeeping.
// Called with the mutex held.
func (s *Service) ensureBootLocked() error {
	err := s.bootLocked()
	// The verdict is the DIRECTORY, not the error: a boot whose reference
	// refresh failed still left a usable directory, while a boot that could
	// not create the directory has nothing at all and must be retried.
	if info, statErr := os.Stat(s.dir); statErr != nil || !info.IsDir() {
		s.bootPending = true
	} else {
		s.bootPending = false
	}
	return err
}

func (s *Service) bootLocked() error {
	if err := os.MkdirAll(s.dir, privateDirPerm); err != nil {
		return fmt.Errorf("soundlib: create sounds dir: %w", err)
	}
	return refreshEmbeddedAsset(s.dir, ReferenceFileName)
}

// retryPendingBoot re-attempts a boot that never produced the directory.
// Cheap and silent in the overwhelmingly common case: one bool read under
// the mutex.
//
// Files() is the retry trigger because it is the one call that always
// happens when a client is actually looking at cues, and because the
// blocker (a file named `sounds`, a read-only parent) is something the user
// fixes out-of-band and then expects to just work.
func (s *Service) retryPendingBoot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bootPending {
		return
	}
	// The error is deliberately dropped: still-blocked is the expected
	// answer, Files() has a fully usable (empty) result without it, and the
	// boot path already logged the original failure.
	_ = s.ensureBootLocked()
}

// Files lists the sounds directory. A missing directory is not an error (a
// boot that has not run EnsureBoot yet, or a user who deleted it); an
// unreadable one, an unreadable or oversized file, and a file that is not a
// canonical cue all land in Warnings with the usable rest intact.
//
// EVERY FILE IS RE-VALIDATED HERE, not only on the way in. The write path is
// not the only way bytes reach this directory: the user can drop a file in,
// an agent can write one, and an editor can rewrite one in place. The
// listing is the last gate before a cue reaches a player, so it is the gate
// that has to hold.
//
// The mutex covers the pending-boot retry only; the directory listing and
// the file reads run unlocked, exactly as theme.Files() and spinner.Files()
// do — a sounds directory on a slow or wedged filesystem must not be able to
// block a write behind a background refetch. `dir` is written once at
// construction, so reading it unlocked is exact for the whole call.
func (s *Service) Files() Files {
	s.retryPendingBoot()

	result := Files{Dir: s.dir, Sounds: []Sound{}, Warnings: []string{}}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not read %s: %v", s.dir, err))
		}
		return result
	}

	ids, invalidNames := collectIDs(entries)
	for _, name := range invalidNames {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: skipped, the name is not <id>.wav (lowercase letters, digits and dashes, starting with a letter or digit, at most 64 characters, lowercase extension)", name))
	}

	for _, id := range ids {
		// The count cap stops the WHOLE listing rather than skipping file by
		// file: past it the directory is not a cue library any more, and one
		// warning explains that better than dozens of identical ones. The
		// ids are sorted, so which cues made the cut is deterministic.
		if len(result.Sounds) >= MaxSounds {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"stopped after %d sounds: %s holds more than this listing carries", MaxSounds, s.dir))
			break
		}
		name := id + FileExt
		wav, warning := readCapped(filepath.Join(s.dir, name), name)
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
			continue
		}
		if err := ValidateWAV(wav); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: skipped, %v", name, err))
			continue
		}
		result.Sounds = append(result.Sounds, Sound{ID: id, WAV: base64.StdEncoding.EncodeToString(wav)})
	}
	return result
}

// MaxWAVBase64Len is the longest base64 body that could decode to a legal
// cue: four characters per three bytes, rounded up to the padded quantum.
const MaxWAVBase64Len = 4 * ((MaxSoundBytes + 2) / 3)

// DecodeWAV turns the base64 body an RPC carries into cue bytes.
//
// The LENGTH IS CHECKED BEFORE DECODING, which is the whole reason this is
// a function rather than a call to base64.StdEncoding.DecodeString at each
// caller: DecodeString allocates three quarters of its input up front, so a
// caller handing over a gigabyte of 'A' would make the host allocate
// 768 MiB before anything could report that the input was absurd.
//
// It does not validate the audio — Put does that, on the decoded bytes.
func DecodeWAV(id, wavBase64 string) ([]byte, error) {
	if len(wavBase64) > MaxWAVBase64Len {
		return nil, fmt.Errorf(
			"soundlib: sound %q is larger than the %d bytes a %d second cue can occupy",
			id, MaxSoundBytes, MaxSoundSeconds)
	}
	wav, err := base64.StdEncoding.DecodeString(wavBase64)
	if err != nil {
		return nil, fmt.Errorf("soundlib: sound %q is not valid base64: %w", id, err)
	}
	return wav, nil
}

// Put writes one validated cue under a new id.
//
// It refuses an EXISTING id rather than overwriting: the id is derived from
// the filename the user picked, two different files easily produce the same
// one, and silently replacing the cue three events are already pointing at
// is not what "add a sound" means. The message says what to do instead.
func (s *Service) Put(id string, wav []byte) error {
	if err := validateID(id); err != nil {
		return err
	}
	if err := ValidateWAV(wav); err != nil {
		return fmt.Errorf("soundlib: %s%s is not a canonical cue: %w", id, FileExt, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, id+FileExt)
	// Lstat, not Stat: a SYMLINK named <id>.wav already occupies the name,
	// and a Stat that followed it to a missing target would report "free"
	// and then let atomicfile's rename replace the link.
	switch _, err := os.Lstat(path); {
	case err == nil:
		return fmt.Errorf("soundlib: a sound named %q already exists; delete it first", id)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("soundlib: inspect %s: %w", path, err)
	}
	count, err := s.countLocked()
	if err != nil {
		return err
	}
	if count >= MaxSounds {
		return fmt.Errorf("soundlib: the sounds directory already holds the maximum of %d sounds; delete one first", MaxSounds)
	}
	if err := atomicfile.Write(path, wav); err != nil {
		return fmt.Errorf("soundlib: write %s: %w", path, err)
	}
	return nil
}

// Delete removes one cue. A cue that is not there is not an error: the
// caller asked for it to be gone and it is gone, and two Settings screens
// deleting the same row must not produce a failure on the second one.
func (s *Service) Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, id+FileExt)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("soundlib: delete %s: %w", path, err)
	}
	return nil
}

// countLocked counts the cue-shaped files currently in the directory. A
// missing directory counts zero — Put's write creates it.
func (s *Service) countLocked() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("soundlib: read %s: %w", s.dir, err)
	}
	ids, _ := collectIDs(entries)
	return len(ids), nil
}

// collectIDs folds a directory listing into the sorted set of cue ids plus
// the sorted set of file names that look like cues but could never be one.
//
// A cue is exactly `<id>.wav`: the id is what every later path is built
// from (Files reads `<id>.wav` back, Put and Delete address it), so an
// upper-case `.WAV` is not a cue on a case-sensitive filesystem and is
// warned about rather than listed under a name that does not exist.
//
// Directories and symlinks are skipped SILENTLY: an entry ending in .wav
// that is really a directory is the user's own filing decision, and
// following a symlink would read a file outside the directory this listing
// claims to describe.
func collectIDs(entries []os.DirEntry) (ids []string, invalidNames []string) {
	ids = make([]string, 0, len(entries))
	invalidNames = make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), FileExt) || !entry.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(name, FileExt)
		if id == name || !idPattern.MatchString(id) {
			invalidNames = append(invalidNames, name)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sort.Strings(invalidNames)
	return ids, invalidNames
}

// validateID refuses anything that is not a cue id. Every public entry
// point runs it, so a traversal attempt or a stray label can never reach a
// filesystem path built from it.
func validateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf(
			"soundlib: %q is not a valid sound id (lowercase letters, digits and dashes, starting with a letter or digit, at most 64 characters)", id)
	}
	return nil
}

// readCapped reads one cue file, or returns the warning explaining why it
// was skipped.
//
// It reads through an io.LimitReader rather than stat-then-ReadFile so an
// oversized file is never fully loaded: the stat form both raced (the file
// can grow between the stat and the read) and pulled the whole thing into
// memory on the second check. Reading one byte past the cap is what
// distinguishes "at the limit" from "over it".
func readCapped(path, name string) (data []byte, warning string) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, MaxSoundBytes+1))
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	if len(data) > MaxSoundBytes {
		return nil, fmt.Sprintf(
			"%s: skipped, it is larger than %d bytes, which is the most %d seconds of canonical audio can occupy",
			name, MaxSoundBytes, MaxSoundSeconds)
	}
	return data, ""
}

// refreshEmbeddedAsset writes assets/<name> into dir when the on-disk copy
// is absent or differs from the embedded one.
func refreshEmbeddedAsset(dir, name string) error {
	want, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return fmt.Errorf("soundlib: read embedded %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	if have, err := os.ReadFile(path); err == nil && string(have) == string(want) {
		return nil
	}
	if err := atomicfile.Write(path, want); err != nil {
		return fmt.Errorf("soundlib: seed %s: %w", name, err)
	}
	return nil
}
