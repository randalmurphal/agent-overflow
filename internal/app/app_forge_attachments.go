package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"agent-overflow/internal/forgeattach"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/transport"
)

// ForgeAttachment is what FetchForgeAttachment answers: the bytes of one
// forge-hosted attachment (an image, video or file a PR/MR body or comment
// references) are fetched through the user's forge CLI on the selected
// computer and served once through the ticketed URL. Kind is what the
// bytes turned out to be by signature, never what the reference claimed.
type ForgeAttachment struct {
	// URL is the relative, single-use, ticketed URL the client fetches the
	// bytes from (internal/transport/attachmentroutes.go).
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	// Kind is "image", "video", "audio" or "file".
	Kind      string `json:"kind"`
	SizeBytes int64  `json:"sizeBytes"`
	Filename  string `json:"filename"`
}

// FetchForgeAttachment resolves one attachment reference found in a PR/MR
// body or comment through the forge CLI (`gh api` / `glab api`), caches the
// bytes, and mints the ticket that serves them.
//
//ao:scope git:operate
//ao:route selected
func (a *App) FetchForgeAttachment(pr gitops.PRReference, href string) (ForgeAttachment, error) {
	entry, err := a.resolveForgeAttachment(pr, href)
	if err != nil {
		return ForgeAttachment{}, err
	}
	server := a.transportServer.Load()
	if server == nil {
		return ForgeAttachment{}, errors.New("forge attachment: transport is not serving")
	}
	url, err := server.MintForgeAttachmentTicket(entry.ID)
	if err != nil {
		return ForgeAttachment{}, err
	}
	return ForgeAttachment{
		URL:       url,
		MimeType:  entry.MimeType,
		Kind:      entry.Kind,
		SizeBytes: int64(len(entry.Data)),
		Filename:  entry.Filename,
	}, nil
}

// SaveForgeAttachment fetches one attachment the same way and writes it to
// the user's Downloads directory on this computer, returning the path.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SaveForgeAttachment(pr gitops.PRReference, href string) (string, error) {
	entry, err := a.resolveForgeAttachment(pr, href)
	if err != nil {
		return "", err
	}
	dir, err := a.downloadsDir()
	if err != nil {
		return "", err
	}
	return writeWithoutOverwriting(dir, downloadFileName(entry.Filename), entry.Data)
}

// resolveForgeAttachment is the one fetch path both bound methods use.
//
// The cache is consulted first and populated after, so a body that
// references the same image in three comments spawns one gh / glab rather
// than three, and a save of something already on screen costs nothing.
func (a *App) resolveForgeAttachment(pr gitops.PRReference, href string) (forgeattach.Entry, error) {
	if a.shuttingDown.Load() {
		return forgeattach.Entry{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return forgeattach.Entry{}, err
	}
	cache := a.forgeAttachments()
	key := forgeattach.CacheKey(pr.Forge, pr.Project(), pr.Number, href)
	if entry, ok := cache.Lookup(key); ok {
		return entry, nil
	}
	data, filename, err := a.gitCore().FetchAttachment("", pr, href, forgeattach.MaxBytes)
	if err != nil {
		return forgeattach.Entry{}, err
	}
	mimeType, kind, err := forgeattach.Classify(data, filename)
	if err != nil {
		return forgeattach.Entry{}, err
	}
	return cache.Put(forgeattach.Entry{
		Key:      key,
		Data:     data,
		MimeType: mimeType,
		Kind:     kind,
		Filename: filename,
	})
}

// forgeAttachments is the lazily constructed byte cache behind both
// methods. Lazy for the reason the keybindings and theme services are:
// a test that builds a bare App gets a working cache without an explicit
// init step, and a boot that never opens a review pane allocates nothing.
func (a *App) forgeAttachments() *forgeattach.Cache {
	a.forgeAttachOnce.Do(func() {
		a.forgeAttachCache = forgeattach.NewCache(forgeattach.DefaultCacheBytes, forgeattach.DefaultTTL)
	})
	return a.forgeAttachCache
}

// OpenForgeAttachment satisfies transport.AttachmentTransfer. The cache
// entry is the whole answer: the bytes were fetched and classified by the
// bound method that minted the ticket, and an id whose entry expired or
// was evicted is a miss the route answers 404.
func (t attachmentTransfer) OpenForgeAttachment(contentID string) (transport.ForgeAttachmentContent, error) {
	entry, ok := t.app.forgeAttachments().Get(contentID)
	if !ok {
		return transport.ForgeAttachmentContent{}, fmt.Errorf("forge attachment %q is no longer cached", contentID)
	}
	return transport.ForgeAttachmentContent{
		MimeType: entry.MimeType,
		Kind:     entry.Kind,
		Filename: entry.Filename,
		ModTime:  entry.StoredAt,
		Content:  nopCloserReader{bytes.NewReader(entry.Data)},
	}, nil
}

// nopCloserReader adapts a retained byte slice onto the ReadSeekCloser
// the route streams. Nothing to release: the cache owns the bytes and
// outlives the response.
type nopCloserReader struct{ *bytes.Reader }

func (nopCloserReader) Close() error { return nil }

// downloadsDir is where a saved attachment lands: the user's own
// Downloads folder when there is one, because that is where a person
// looks for a file they just saved. The app-private fallback exists for a
// headless or minimal home where it does not, and is created rather than
// assumed.
func (a *App) downloadsDir() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidate := filepath.Join(home, "Downloads")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate, nil
		}
	}
	if a.configDir == "" {
		return "", errors.New("app data directory is not initialised")
	}
	dir := filepath.Join(a.configDir, "downloads")
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}
	return dir, nil
}

// maxDownloadCollisions bounds the " (2)" walk. A directory already
// holding this many copies of one name is a caller in a loop, and
// answering an error is better than the walk becoming the cost.
const maxDownloadCollisions = 200

// writeWithoutOverwriting creates the file exclusively, so a name already
// in Downloads is never clobbered: the suffix walk is the same " (2)"
// convention a browser uses, and O_EXCL is what makes the check and the
// create one operation rather than a race.
func writeWithoutOverwriting(dir, name string, data []byte) (string, error) {
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for attempt := 1; attempt <= maxDownloadCollisions; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, attempt, extension)
		}
		path := filepath.Join(dir, candidate)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("save attachment: %w", err)
		}
		if _, err := file.Write(data); err != nil {
			file.Close()
			os.Remove(path)
			return "", fmt.Errorf("save attachment: %w", err)
		}
		if err := file.Close(); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("save attachment: %w", err)
		}
		return path, nil
	}
	return "", fmt.Errorf("save attachment: %d files named like %q already exist", maxDownloadCollisions, name)
}

const (
	downloadStemMaxRunes      = 80
	downloadExtensionMaxRunes = 16
)

// downloadFileName makes one forge filename safe to create in a
// directory the user browses.
//
// Unlike sanitizeCIFileSegment this keeps Unicode letters and the
// extension: the name came from the person who uploaded the file, and a
// screenshot called "Скриншот.png" should still be that file after a
// save rather than a row of dashes. What it removes is everything that
// decides where a file goes or how a shell reads it.
func downloadFileName(name string) string {
	name = strings.TrimSpace(name)
	// Both separators, not the platform's: the name crossed from a forge,
	// so a Windows-shaped one has to be cut on Linux too.
	name = name[strings.LastIndexAny(name, `/\`)+1:]

	// The separating dot is re-added rather than sanitized through: a
	// leading dot is exactly what the segment cleaner strips, and losing
	// it would turn every save into an extensionless file.
	raw := filepath.Ext(name)
	extension := sanitizeDownloadSegment(strings.TrimPrefix(raw, "."), downloadExtensionMaxRunes)
	if extension != "" {
		extension = "." + extension
	}
	stem := sanitizeDownloadSegment(strings.TrimSuffix(name, raw), downloadStemMaxRunes)
	if stem == "" {
		stem = "attachment"
	}
	return stem + extension
}

func sanitizeDownloadSegment(segment string, maxRunes int) string {
	var b strings.Builder
	count := 0
	for _, r := range segment {
		if count >= maxRunes {
			break
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r),
			r == '.', r == '-', r == '_', r == ' ', r == '(', r == ')':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		count++
	}
	// A leading dot would hide the file; a trailing one is invalid on
	// Windows. Spaces at either end survive no filesystem usefully.
	return strings.Trim(b.String(), " .-")
}
