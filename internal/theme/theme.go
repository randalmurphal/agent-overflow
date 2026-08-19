package theme

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"agent-overflow/internal/atomicfile"
)

// assets holds the generated reference artifacts seeded into the user's
// themes directory at boot. Both files are produced by
// frontend/scripts/generate-theme-reference.mjs from the frontend token
// registry and committed here; the app treats the embedded copy as
// authoritative and refreshes the on-disk copy whenever it differs.
//
//go:embed assets/*
var assets embed.FS

const (
	// DirName is the themes directory's basename inside <configDir>.
	DirName = "themes"
	// AppearanceFileName holds the selection (mode + one theme id per
	// axis + the window-background cache).
	AppearanceFileName = "appearance.json"
	// SchemaFileName and TokensFileName are the generated reference
	// artifacts seeded from the embedded assets.
	SchemaFileName = "theme.schema.json"
	TokensFileName = "TOKENS.md"

	// MaxThemeFileBytes caps a single theme file. Anything larger is
	// reported as a warning and skipped rather than piped to the
	// frontend — the whole listing crosses one RPC.
	MaxThemeFileBytes = 1 << 20

	// MaxThemeFilesBytes and MaxThemeFiles cap the LISTING, not a file.
	// The per-file cap alone bounds nothing that matters: a directory of
	// 500 files just under it would build a half-gigabyte RPC result and
	// hand the frontend a parse job per entry. These two are what make
	// the answer's size a property of the format rather than of whatever
	// the user (or a runaway agent) left in the directory. Both are
	// generous next to any real themes directory — a hand-written theme
	// is single-digit KiB.
	MaxThemeFilesBytes = 4 << 20
	MaxThemeFiles      = 64
)

// Defaults for the appearance selection. `default` is the built-in UI
// theme (the app.css palette) and `github` the built-in code theme
// (syntax.css); both are identity themes emitting no CSS.
const (
	DefaultMode      = "system"
	DefaultUITheme   = "default"
	DefaultCodeTheme = "github"
)

// privateDirPerm matches internal/atomicfile's directory mode: every
// write here goes through that package, and the directory it creates on
// demand must not disagree with the one EnsureBoot creates up front.
const privateDirPerm os.FileMode = 0o700

// idPattern is the theme-id shape: filename stems are kebab-case ASCII.
// Anchored and length-bounded so an id can never escape its directory or
// blow up a CSS selector on the frontend.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// hexPattern is the windowBackground shape — the only color value the
// backend ever inspects. Everything else in a theme is opaque text.
var hexPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// allowedModes is the mode enum. Kept as a slice so the error message
// can name the valid values.
var allowedModes = []string{"system", "light", "dark"}

// Appearance is the selection persisted in themes/appearance.json.
//
// WindowBackground is a CACHE the frontend maintains (the last resolved
// surface-0-family color) so the native window can be constructed with
// the right color before the webview paints. It is never user-edited
// semantics, which is why it is the one omitempty field: absent means
// "no cached value yet", not "black".
type Appearance struct {
	Mode             string `json:"mode,omitempty"`
	UITheme          string `json:"uiTheme,omitempty"`
	CodeTheme        string `json:"codeTheme,omitempty"`
	WindowBackground string `json:"windowBackground,omitempty"`
}

// DefaultAppearance is the selection a fresh install gets.
func DefaultAppearance() Appearance {
	return Appearance{Mode: DefaultMode, UITheme: DefaultUITheme, CodeTheme: DefaultCodeTheme}
}

// File is one theme file as it crosses the wire: an id (the filename
// stem) and the file's bytes verbatim.
//
// Raw is deliberately a string and not a parsed structure. Go never
// parses theme JSON — the token vocabulary lives in the frontend token
// registry, and a backend that understood it would be a second,
// silently-drifting definition of the format (root CLAUDE.md principle
// 1). The frontend parses, validates per-token, and reports.
type File struct {
	ID  string `json:"id"`
	Raw string `json:"raw"`
}

// Files is the single theme RPC's answer: where the directory is, every
// readable theme file, the current selection, and every reason a file or
// the directory could not be fully read.
//
// Warnings are DATA, not log lines: a theme file the user just wrote
// that is too large or unreadable has to say so in the UI, because the
// symptom otherwise is "my theme silently does nothing".
type Files struct {
	Dir        string     `json:"dir"`
	Themes     []File     `json:"themes"`
	Appearance Appearance `json:"appearance"`
	Warnings   []string   `json:"warnings"`
}

// Service owns <configDir>/themes and serializes reads and writes
// against it under a private mutex.
type Service struct {
	mu  sync.Mutex
	dir string

	// bootPending records that EnsureBoot ran and appearance.json still
	// does not exist — the themes directory could not be created (a FILE
	// sitting on the path, a read-only parent, a full disk). pendingLegacy
	// holds the retiring settings.theme value that boot was carrying.
	//
	// This pair exists because the legacy value is consumed EXACTLY ONCE:
	// the next sparse settings write drops the retired key, so a boot that
	// failed to seed has permanently lost the user's light/dark choice
	// unless this process keeps it. Files() retries with it, so the seed
	// heals the moment the blocker is removed — for as long as the process
	// lives, which is the whole window in which the value still exists
	// anywhere.
	bootPending   bool
	pendingLegacy string
}

// New returns a Service rooted at <configDir>/themes. When configDir is
// empty it falls back to ~/.agent-overflow/themes so an early-boot RPC
// still resolves a writable path — the same fallback keybindings.New
// uses. Returns an error only when neither path is available.
func New(configDir string) (*Service, error) {
	if strings.TrimSpace(configDir) != "" {
		return &Service{dir: filepath.Join(configDir, DirName)}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("theme: cannot determine home directory: %w", err)
	}
	return &Service{dir: filepath.Join(home, ".agent-overflow", DirName)}, nil
}

// Dir returns the absolute themes-directory path.
func (s *Service) Dir() string { return s.dir }

// AppearancePath returns the absolute appearance.json path. The watcher
// uses it to recognise (and ignore) this process's own writes.
func (s *Service) AppearancePath() string { return filepath.Join(s.dir, AppearanceFileName) }

// EnsureBoot creates the themes directory, refreshes the generated
// reference artifacts from the embedded copies, and seeds
// appearance.json when it is absent.
//
// legacyMode is the retiring settings.theme value. It is consulted ONLY
// when appearance.json does not exist yet, so a user upgrading into the
// theme system keeps the light/dark choice they already made. An empty
// or unrecognised value falls through to DefaultMode.
//
// The reference artifacts are refreshed rather than merely seeded
// because they are GENERATED: the embedded copy is what this build's
// token vocabulary actually is, so a stale on-disk copy would document
// tokens that no longer exist to the agent editing the theme. Local
// edits to those two files are therefore expected to be lost; the
// header line in each says so.
// A failed boot is REMEMBERED rather than lost: see the bootPending
// field and retryPendingBoot below.
func (s *Service) EnsureBoot(legacyMode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureBootLocked(legacyMode)
}

// ensureBootLocked is EnsureBoot's body plus the pending-boot bookkeeping.
// Called with the mutex held.
func (s *Service) ensureBootLocked(legacyMode string) error {
	err := s.bootLocked(legacyMode)
	// The verdict is the FILE, not the error: a boot whose asset refresh
	// failed but whose seed landed has nothing left to carry, while a boot
	// that could not create the directory must keep the legacy value alive.
	if _, statErr := os.Stat(s.AppearancePath()); statErr != nil {
		s.bootPending = true
		s.pendingLegacy = legacyMode
	} else {
		s.bootPending = false
		s.pendingLegacy = ""
	}
	return err
}

func (s *Service) bootLocked(legacyMode string) error {
	if err := ensurePrivateDir(s.dir); err != nil {
		return fmt.Errorf("theme: create themes dir: %w", err)
	}
	var errs []error
	for _, name := range []string{SchemaFileName, TokensFileName} {
		if err := refreshEmbeddedAsset(s.dir, name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.seedAppearance(legacyMode); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// retryPendingBoot re-attempts a boot that never produced appearance.json,
// carrying the same legacy mode the original attempt had. Cheap and silent
// in the overwhelmingly common case: one map-free bool read under the
// mutex.
//
// Files() is the retry trigger because it is the one call that always
// happens when a client is actually looking at themes, and because the
// blocker (a file named `themes`, a read-only parent) is something the
// user fixes out-of-band and then expects to just work — not something
// worth failing an RPC over.
func (s *Service) retryPendingBoot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bootPending {
		return
	}
	if _, err := os.Stat(s.AppearancePath()); err == nil {
		s.bootPending = false
		s.pendingLegacy = ""
		return
	}
	// The error is deliberately dropped: still-blocked is the expected
	// answer, Files() has a fully usable result without it, and the boot
	// path already logged the original failure.
	_ = s.ensureBootLocked(s.pendingLegacy)
}

// seedAppearance writes appearance.json only when it is absent. Called
// with the mutex held.
func (s *Service) seedAppearance(legacyMode string) error {
	path := s.AppearancePath()
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("theme: inspect %s: %w", AppearanceFileName, err)
	}
	seed := DefaultAppearance()
	if mode := strings.TrimSpace(legacyMode); isAllowedMode(mode) {
		seed.Mode = mode
	}
	return writeAppearance(path, seed)
}

// Files lists the themes directory. A missing directory is not an error
// (it is a boot that has not run EnsureBoot yet, or a user who deleted
// it); an unreadable one, an unreadable file, an oversized file, a
// listing that exceeds the aggregate caps, and a malformed
// appearance.json all land in Warnings with the usable rest of the answer
// intact.
//
// The mutex covers the pending-boot retry and the dir snapshot ONLY; the
// directory listing and the file reads run unlocked. A themes directory
// on a slow or wedged filesystem (a network mount, a spun-down disk) must
// not be able to block SetAppearance — the user changing a theme is an
// interactive action, and the read it would queue behind is a background
// refetch. `dir` is written once at construction, so the snapshot is
// exact for the whole call.
func (s *Service) Files() Files {
	s.retryPendingBoot()

	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()

	result := Files{Dir: dir, Themes: []File{}, Appearance: DefaultAppearance(), Warnings: []string{}}

	appearance, warning := readAppearance(filepath.Join(dir, AppearanceFileName))
	result.Appearance = appearance
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not read %s: %v", dir, err))
		}
		return result
	}

	totalBytes := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		if strings.EqualFold(name, AppearanceFileName) || strings.EqualFold(name, SchemaFileName) {
			continue
		}
		// Directories and symlinks are skipped silently: a themes/
		// directory ending in .json is a user's own filing decision, and
		// following a symlink would read a file outside the directory
		// the RPC claims to describe.
		if !entry.Type().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !idPattern.MatchString(id) {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: skipped, the filename stem is not a valid theme id (lowercase letters, digits and dashes, starting with a letter or digit, at most 64 characters)", name))
			continue
		}
		// The aggregate caps stop the WHOLE listing rather than skipping
		// file by file: past them the directory is not a themes directory
		// any more, and one warning explains that better than hundreds of
		// identical ones would. ReadDir sorts by filename, so which files
		// made the cut is deterministic.
		if len(result.Themes) >= MaxThemeFiles {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"stopped after %d theme files: %s holds more than this listing carries", MaxThemeFiles, dir))
			break
		}
		data, warning := readThemeFile(filepath.Join(dir, name), name)
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
			continue
		}
		if totalBytes+len(data) > MaxThemeFilesBytes {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"stopped at %s: the theme files ahead of it already total the %s this listing carries", name, humanBytes(MaxThemeFilesBytes)))
			break
		}
		totalBytes += len(data)
		result.Themes = append(result.Themes, File{ID: id, Raw: string(data)})
	}
	sort.Slice(result.Themes, func(i, j int) bool { return result.Themes[i].ID < result.Themes[j].ID })
	return result
}

// readThemeFile reads one theme file, or returns the warning explaining
// why it was skipped.
//
// It reads through an io.LimitReader rather than stat-then-ReadFile so an
// oversized file is never fully loaded: the stat form both raced (the file
// can grow between the stat and the read) and pulled the whole thing into
// memory on the second check, which is the exact cost the cap exists to
// avoid. Reading one byte past the cap is what distinguishes "at the
// limit" from "over it".
func readThemeFile(path, name string) (data []byte, warning string) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, MaxThemeFileBytes+1))
	if err != nil {
		return nil, fmt.Sprintf("%s: skipped, %v", name, err)
	}
	if len(data) > MaxThemeFileBytes {
		return nil, oversizeWarning(name)
	}
	return data, ""
}

// oversizeWarning is the one wording for "this file is past the per-file
// cap" — it had drifted into two copies with different numbers in them.
func oversizeWarning(name string) string {
	return fmt.Sprintf("%s: skipped, it exceeds the %s theme file limit", name, humanBytes(MaxThemeFileBytes))
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

// SetAppearance validates and atomically persists the selection.
//
// Validation is loud on purpose: an out-of-enum mode or a malformed id
// would leave the frontend resolving against something that does not
// exist, and silently coercing it would make the user's next read
// disagree with what they just saved. Empty fields are the one
// exception — they normalize onto the defaults, so a caller may send
// `{mode: "dark"}` without restating the axes it does not care about.
func (s *Service) SetAppearance(appearance Appearance) error {
	normalized, err := NormalizeAppearance(appearance)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensurePrivateDir(s.dir); err != nil {
		return fmt.Errorf("theme: create themes dir: %w", err)
	}
	if err := writeAppearance(s.AppearancePath(), normalized); err != nil {
		return err
	}
	// An explicit selection retires the pending legacy mode outright: the
	// user just said what they want, which is strictly newer than the
	// value the migration was carrying.
	s.bootPending = false
	s.pendingLegacy = ""
	return nil
}

// NormalizeAppearance trims, defaults, and validates a selection. It is
// the one definition of "a valid appearance" — SetAppearance and the
// tests both go through it.
func NormalizeAppearance(appearance Appearance) (Appearance, error) {
	out := Appearance{
		Mode:             strings.TrimSpace(appearance.Mode),
		UITheme:          strings.TrimSpace(appearance.UITheme),
		CodeTheme:        strings.TrimSpace(appearance.CodeTheme),
		WindowBackground: strings.TrimSpace(appearance.WindowBackground),
	}
	if out.Mode == "" {
		out.Mode = DefaultMode
	}
	if out.UITheme == "" {
		out.UITheme = DefaultUITheme
	}
	if out.CodeTheme == "" {
		out.CodeTheme = DefaultCodeTheme
	}
	if !isAllowedMode(out.Mode) {
		return Appearance{}, fmt.Errorf("theme: mode %q is not one of %s", out.Mode, strings.Join(allowedModes, ", "))
	}
	if !idPattern.MatchString(out.UITheme) {
		return Appearance{}, fmt.Errorf("theme: uiTheme %q is not a valid theme id", out.UITheme)
	}
	if !idPattern.MatchString(out.CodeTheme) {
		return Appearance{}, fmt.Errorf("theme: codeTheme %q is not a valid theme id", out.CodeTheme)
	}
	if out.WindowBackground != "" && !hexPattern.MatchString(out.WindowBackground) {
		return Appearance{}, fmt.Errorf("theme: windowBackground %q is not a #rrggbb color", out.WindowBackground)
	}
	return out, nil
}

// ParseHexColor turns a validated #rrggbb string into 8-bit channels.
// The one place the backend interprets a color value — the native
// window background is an OS-level call, not CSS.
func ParseHexColor(value string) (red, green, blue uint8, err error) {
	trimmed := strings.TrimSpace(value)
	if !hexPattern.MatchString(trimmed) {
		return 0, 0, 0, fmt.Errorf("theme: %q is not a #rrggbb color", value)
	}
	parsed, err := strconv.ParseUint(trimmed[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("theme: parse %q: %w", value, err)
	}
	return uint8(parsed >> 16), uint8(parsed >> 8), uint8(parsed), nil
}

// WindowBackground reads just the cached native-window color out of
// <configDir>/themes/appearance.json.
//
// Deliberately a free function and not a Service method: the desktop
// entry point calls it BEFORE the App service exists, once, on the boot
// path, and must stay cheap — one small file read, no directory
// creation, no listing. Every failure (no config dir, absent file,
// malformed JSON, non-hex value) answers "" and the caller keeps its
// compiled-in fallback color.
func WindowBackground(configDir string) string {
	if strings.TrimSpace(configDir) == "" {
		return ""
	}
	appearance, warning := readAppearance(filepath.Join(configDir, DirName, AppearanceFileName))
	if warning != "" {
		return ""
	}
	if !hexPattern.MatchString(appearance.WindowBackground) {
		return ""
	}
	return appearance.WindowBackground
}

// readAppearance loads the selection, filling absent fields with the
// defaults. Returns the defaults plus a human-readable warning when the
// file exists but cannot be read or parsed; an absent file is a fresh
// install and warns about nothing.
func readAppearance(path string) (Appearance, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultAppearance(), ""
		}
		return DefaultAppearance(), fmt.Sprintf("could not read %s: %v", path, err)
	}
	if len(data) == 0 {
		return DefaultAppearance(), ""
	}
	var parsed Appearance
	if err := json.Unmarshal(data, &parsed); err != nil {
		return DefaultAppearance(), fmt.Sprintf("could not parse %s: %v; using the default appearance", path, err)
	}
	// A hand-edited file is normalized, not rejected: the read path has
	// no user to refuse, and answering with the defaults for the fields
	// that are wrong beats answering with nothing.
	normalized, err := NormalizeAppearance(parsed)
	if err != nil {
		return DefaultAppearance(), fmt.Sprintf("%s: %v; using the default appearance", path, err)
	}
	return normalized, ""
}

// writeAppearance persists the selection atomically (temp file + fsync +
// rename, 0600) so a crash mid-write cannot corrupt it. The trailing
// newline is why this marshals itself instead of calling
// atomicfile.WriteJSON — appearance.json is a file users and agents open
// in an editor.
func writeAppearance(path string, appearance Appearance) error {
	data, err := json.MarshalIndent(appearance, "", "  ")
	if err != nil {
		return fmt.Errorf("theme: marshal appearance: %w", err)
	}
	return atomicfile.Write(path, append(data, '\n'))
}

// refreshEmbeddedAsset writes assets/<name> into dir when the on-disk
// copy is absent or differs from the embedded one.
func refreshEmbeddedAsset(dir, name string) error {
	want, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return fmt.Errorf("theme: read embedded %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	if have, err := os.ReadFile(path); err == nil && string(have) == string(want) {
		return nil
	}
	if err := atomicfile.Write(path, want); err != nil {
		return fmt.Errorf("theme: seed %s: %w", name, err)
	}
	return nil
}

func ensurePrivateDir(path string) error {
	return os.MkdirAll(path, privateDirPerm)
}

func isAllowedMode(mode string) bool {
	for _, allowed := range allowedModes {
		if mode == allowed {
			return true
		}
	}
	return false
}
