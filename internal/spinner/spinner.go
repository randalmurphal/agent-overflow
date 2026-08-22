package spinner

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

// assets holds the authoring reference seeded into the user's spinners
// directory at boot. The embedded copy is authoritative and the on-disk
// copy is refreshed whenever it differs — it documents THIS build's
// contract, so a stale copy would teach an agent a format that is no
// longer read.
//
//go:embed assets/*
var assets embed.FS

const (
	// DirName is the spinners directory's basename inside <configDir>.
	DirName = "spinners"
	// ReferenceFileName is the generated authoring doc seeded from the
	// embedded assets. Named in caps like themes' TOKENS.md because it is
	// documentation sitting in a directory of content files.
	ReferenceFileName = "SPINNERS.md"

	// MaxSpritePNGBytes and MaxManifestBytes cap ONE file each. A sprite
	// strip is pixels — an eight-frame 72px-tall row of transparent PNG is
	// tens of KiB — and a manifest is two integers, so both caps are
	// several orders of magnitude above anything the format needs. They
	// exist because the whole listing crosses one RPC.
	MaxSpritePNGBytes = 4 << 20
	MaxManifestBytes  = 16 << 10

	// MaxSpritesBytes and MaxSprites cap the LISTING, not a file. The
	// per-file caps alone bound nothing that matters: 500 sprites just
	// under the PNG cap would build a two-gigabyte RPC result — and the
	// PNG travels base64, so every byte counted here is four thirds of a
	// byte on the wire. These two are what make the answer's size a
	// property of the format rather than of whatever the user (or a
	// runaway agent) left in the directory.
	MaxSpritesBytes = 24 << 20
	MaxSprites      = 32
)

// privateDirPerm matches internal/atomicfile's directory mode: the
// reference-asset write goes through that package, and the directory it
// creates on demand must not disagree with the one EnsureBoot creates up
// front.
const privateDirPerm os.FileMode = 0o700

// idPattern is the sprite-id shape: filename stems are kebab-case ASCII,
// the same rule theme ids follow. Anchored and length-bounded so an id
// can never escape its directory or blow up a CSS identifier on the
// frontend.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Sprite is one custom spinner as it crosses the wire: the id both files
// share, the sidecar manifest verbatim, and the strip's PNG bytes
// base64-encoded.
//
// Manifest is deliberately a string and not a parsed structure. Go never
// parses it — the animation vocabulary lives in the frontend, and a
// backend that understood it would be a second, silently-drifting
// definition of the format (root CLAUDE.md principle 1). The frontend
// parses, validates, and reports.
//
// PNG carries the strip BASE64-ENCODED, and is a string rather than a
// []byte for a mechanical reason: encoding/json base64-encodes a []byte
// at runtime, but the Wails binding generator has no special case for it
// and would emit `number[]` in TypeScript — a declared type that
// disagrees with what the wire actually carries. Every other binary
// payload in this app (GetAttachmentData, AttachmentThumbnail.Data,
// terminal replay) already spells base64 out for the same reason; this
// follows them.
type Sprite struct {
	ID       string `json:"id"`
	Manifest string `json:"manifest"`
	PNG      string `json:"png"`
}

// Files is the single spinner RPC's answer: where the directory is,
// every readable sprite, and every reason a file or the directory could
// not be fully read.
//
// Warnings are DATA, not log lines: a strip the user just dropped in
// that is missing its sidecar, oversized, or unreadable has to say so in
// the UI, because the symptom otherwise is "my spinner silently does
// nothing".
type Files struct {
	Dir      string   `json:"dir"`
	Sprites  []Sprite `json:"sprites"`
	Warnings []string `json:"warnings"`
}

// Service owns <configDir>/spinners and serializes boot against it under
// a private mutex.
type Service struct {
	mu  sync.Mutex
	dir string

	// bootPending records that EnsureBoot ran and the spinners directory
	// still does not exist — something is sitting on the path (a FILE with
	// that name, a read-only parent, a full disk). Files() retries, so the
	// seed heals the moment the blocker is removed rather than waiting for
	// the next launch.
	//
	// Unlike theme.Service there is no value to carry across the retry:
	// nothing is being migrated here, so the retry is just the seed again.
	bootPending bool
}

// New returns a Service rooted at <configDir>/spinners. When configDir is
// empty it falls back to ~/.agent-overflow/spinners so an early-boot RPC
// still resolves a writable path — the same fallback theme.New and
// keybindings.New use. Returns an error only when neither path is
// available.
func New(configDir string) (*Service, error) {
	if strings.TrimSpace(configDir) != "" {
		return &Service{dir: filepath.Join(configDir, DirName)}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("spinner: cannot determine home directory: %w", err)
	}
	return &Service{dir: filepath.Join(home, ".agent-overflow", DirName)}, nil
}

// Dir returns the absolute spinners-directory path.
func (s *Service) Dir() string { return s.dir }

// EnsureBoot creates the spinners directory and refreshes the generated
// authoring reference from the embedded copy.
//
// The reference is refreshed rather than merely seeded because it is
// GENERATED: the embedded copy is what this build's sprite contract
// actually is, so a stale on-disk copy would document a format the app
// no longer reads to the agent authoring a sprite. Local edits to it are
// therefore expected to be lost; its first line says so.
//
// A failed boot is REMEMBERED rather than lost: see bootPending and
// retryPendingBoot below.
func (s *Service) EnsureBoot() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureBootLocked()
}

// ensureBootLocked is EnsureBoot's body plus the pending-boot
// bookkeeping. Called with the mutex held.
func (s *Service) ensureBootLocked() error {
	err := s.bootLocked()
	// The verdict is the DIRECTORY, not the error: a boot whose asset
	// refresh failed still left a usable directory the user can drop
	// sprites into, while a boot that could not create the directory has
	// nothing at all and must be retried.
	if info, statErr := os.Stat(s.dir); statErr != nil || !info.IsDir() {
		s.bootPending = true
	} else {
		s.bootPending = false
	}
	return err
}

func (s *Service) bootLocked() error {
	if err := ensurePrivateDir(s.dir); err != nil {
		return fmt.Errorf("spinner: create spinners dir: %w", err)
	}
	return refreshEmbeddedAsset(s.dir, ReferenceFileName)
}

// retryPendingBoot re-attempts a boot that never produced the directory.
// Cheap and silent in the overwhelmingly common case: one bool read under
// the mutex.
//
// Files() is the retry trigger because it is the one call that always
// happens when a client is actually looking at spinners, and because the
// blocker (a file named `spinners`, a read-only parent) is something the
// user fixes out-of-band and then expects to just work — not something
// worth failing an RPC over.
func (s *Service) retryPendingBoot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bootPending {
		return
	}
	// The error is deliberately dropped: still-blocked is the expected
	// answer, Files() has a fully usable (empty) result without it, and
	// the boot path already logged the original failure.
	_ = s.ensureBootLocked()
}

// Files lists the spinners directory. A missing directory is not an error
// (it is a boot that has not run EnsureBoot yet, or a user who deleted
// it); an unreadable one, an unreadable or oversized file, a half-written
// sprite, and a listing that exceeds the aggregate caps all land in
// Warnings with the usable rest of the answer intact.
//
// The mutex covers the pending-boot retry and the dir snapshot ONLY; the
// directory listing and the file reads run unlocked, exactly as
// theme.Files() does — a spinners directory on a slow or wedged
// filesystem (a network mount, a spun-down disk) must not be able to
// block the boot path behind a background refetch. `dir` is written once
// at construction, so the snapshot is exact for the whole call.
func (s *Service) Files() Files {
	s.retryPendingBoot()

	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()

	result := Files{Dir: dir, Sprites: []Sprite{}, Warnings: []string{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not read %s: %v", dir, err))
		}
		return result
	}

	pairs, invalidIDs := collectPairs(entries)
	for _, stem := range invalidIDs {
		// %q, not %s: a dotfile named exactly ".png" has an EMPTY stem, and
		// a bare-%s warning about it would open with no subject at all.
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%q: skipped, the filename stem is not a valid spinner id (lowercase letters, digits and dashes, starting with a letter or digit, at most 64 characters)", stem))
	}

	ids := make([]string, 0, len(pairs))
	for id := range pairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	totalBytes := 0
	for _, id := range ids {
		// A sprite is a PAIR. Half of one is the single most likely thing
		// to be sitting in this directory — a strip dropped in before its
		// sidecar was written, or a sidecar whose strip was renamed — and
		// it is invisible without a warning that names the missing file.
		if missing := pairs[id].missing(id); missing != "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: skipped, a sprite needs both %s.png and %s.json and %s is missing", id, id, id, missing))
			continue
		}
		// The aggregate caps stop the WHOLE listing rather than skipping
		// sprite by sprite: past them the directory is not a spinners
		// directory any more, and one warning explains that better than
		// dozens of identical ones would. The ids are sorted, so which
		// sprites made the cut is deterministic.
		if len(result.Sprites) >= MaxSprites {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"stopped after %d sprites: %s holds more than this listing carries", MaxSprites, dir))
			break
		}
		png, warning := readCapped(filepath.Join(dir, id+".png"), id+".png", MaxSpritePNGBytes)
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
			continue
		}
		manifest, warning := readCapped(filepath.Join(dir, id+".json"), id+".json", MaxManifestBytes)
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
			continue
		}
		if totalBytes+len(png)+len(manifest) > MaxSpritesBytes {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"stopped at %s: the sprites ahead of it already total the %s this listing carries", id, humanBytes(MaxSpritesBytes)))
			break
		}
		totalBytes += len(png) + len(manifest)
		result.Sprites = append(result.Sprites, Sprite{
			ID:       id,
			Manifest: string(manifest),
			PNG:      base64.StdEncoding.EncodeToString(png),
		})
	}
	return result
}

// spritePair records which halves of one sprite were seen in the
// directory listing.
type spritePair struct {
	png      bool
	manifest bool
}

// missing names the half that is absent, or "" when the pair is whole.
func (p spritePair) missing(id string) string {
	switch {
	case !p.png:
		return id + ".png"
	case !p.manifest:
		return id + ".json"
	default:
		return ""
	}
}

// collectPairs folds a directory listing into per-id pair state plus the
// sorted set of stems that could never be an id.
//
// Directories and symlinks are skipped SILENTLY: a spinners/ entry ending
// in .png that is really a directory is the user's own filing decision,
// and following a symlink would read a file outside the directory the RPC
// claims to describe.
func collectPairs(entries []os.DirEntry) (map[string]*spritePair, []string) {
	pairs := make(map[string]*spritePair)
	invalid := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if strings.EqualFold(name, ReferenceFileName) {
			continue
		}
		extension := strings.ToLower(filepath.Ext(name))
		if extension != ".png" && extension != ".json" {
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		id := name[:len(name)-len(filepath.Ext(name))]
		if !idPattern.MatchString(id) {
			// One warning per STEM, not per file: a `My Sprite.png` beside a
			// `My Sprite.json` is one mistake, and saying so twice reads
			// like two.
			invalid[id] = struct{}{}
			continue
		}
		pair := pairs[id]
		if pair == nil {
			pair = &spritePair{}
			pairs[id] = pair
		}
		if extension == ".png" {
			pair.png = true
		} else {
			pair.manifest = true
		}
	}
	stems := make([]string, 0, len(invalid))
	for stem := range invalid {
		stems = append(stems, stem)
	}
	sort.Strings(stems)
	return pairs, stems
}

// readCapped reads one file, or returns the warning explaining why it was
// skipped.
//
// It reads through an io.LimitReader rather than stat-then-ReadFile so an
// oversized file is never fully loaded: the stat form both raced (the file
// can grow between the stat and the read) and pulled the whole thing into
// memory on the second check, which is the exact cost the cap exists to
// avoid. Reading one byte past the cap is what distinguishes "at the
// limit" from "over it".
func readCapped(path, name string, limit int) (data []byte, warning string) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	if len(data) > limit {
		return nil, fmt.Sprintf("%s: skipped, it exceeds the %s limit", name, humanBytes(limit))
	}
	return data, ""
}

// humanBytes renders a byte cap the way a warning should read. The caps
// are all whole binary multiples, so this stays exact rather than
// approximate.
func humanBytes(bytes int) string {
	switch {
	case bytes >= 1<<20 && bytes%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", bytes/(1<<20))
	case bytes >= 1<<10 && bytes%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", bytes/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// refreshEmbeddedAsset writes assets/<name> into dir when the on-disk
// copy is absent or differs from the embedded one.
func refreshEmbeddedAsset(dir, name string) error {
	want, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return fmt.Errorf("spinner: read embedded %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	if have, err := os.ReadFile(path); err == nil && string(have) == string(want) {
		return nil
	}
	if err := atomicfile.Write(path, want); err != nil {
		return fmt.Errorf("spinner: seed %s: %w", name, err)
	}
	return nil
}

func ensurePrivateDir(path string) error {
	return os.MkdirAll(path, privateDirPerm)
}
