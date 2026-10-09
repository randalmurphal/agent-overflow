package app

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/attachment"
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
	Kind string `json:"kind"`
	// SizeBytes is the original attachment's byte count, whichever bytes
	// URL serves.
	SizeBytes int64  `json:"sizeBytes"`
	Filename  string `json:"filename"`
	// Width and Height are the served image's pixel size, so the client
	// reserves the box before the bytes decode; zero when unknown (svg,
	// ico, avif) and for every other kind.
	Width  int `json:"width"`
	Height int `json:"height"`
	// OriginalWidth and OriginalHeight are the original image's pixel
	// size, zero when unknown.
	OriginalWidth  int `json:"originalWidth"`
	OriginalHeight int `json:"originalHeight"`
	// Derived is false when URL serves the attachment's own bytes.
	Derived bool `json:"derived"`
}

// FetchForgeAttachment resolves one attachment reference found in a PR/MR
// body or comment through the forge CLI (`gh api` / `glab api`), caches the
// bytes, and mints the ticket that serves them. For an image, a positive
// maxWidth (device pixels) serves a derivative at the next ladder width when
// that is smaller than the original (attachment.Derive); 0 serves the
// original.
//
//ao:scope git:operate
//ao:route selected
func (a *App) FetchForgeAttachment(pr gitops.PRReference, href string, maxWidth int) (ForgeAttachment, error) {
	original, err := a.resolveForgeAttachment(pr, href)
	if err != nil {
		return ForgeAttachment{}, err
	}
	served, err := a.forgeAttachmentAt(original, maxWidth)
	if err != nil {
		return ForgeAttachment{}, err
	}
	server := a.transportServer.Load()
	if server == nil {
		return ForgeAttachment{}, errors.New("forge attachment: transport is not serving")
	}
	url, err := server.MintForgeAttachmentTicket(served.ID)
	if err != nil {
		return ForgeAttachment{}, err
	}
	return ForgeAttachment{
		URL:            url,
		MimeType:       served.Value.MimeType,
		Kind:           served.Value.Kind,
		SizeBytes:      int64(len(original.Value.Data)),
		Filename:       original.Value.Filename,
		Width:          served.Value.Width,
		Height:         served.Value.Height,
		OriginalWidth:  original.Value.Width,
		OriginalHeight: original.Value.Height,
		Derived:        served.ID != original.ID,
	}, nil
}

// forgeAttachmentAt answers the entry that serves an attachment at maxWidth:
// the original, or an image derivative held in the same cache. The
// derivative's key carries the original's content id, so a re-fetched
// original can never pair with a derivative made from earlier bytes.
func (a *App) forgeAttachmentAt(original forgeattach.Entry, maxWidth int) (forgeattach.Entry, error) {
	tier := attachment.DeriveTier(maxWidth)
	if original.Value.Kind != forgeattach.KindImage || tier == 0 {
		return original, nil
	}
	cache := a.forgeAttachments()
	identity := original.Key + "\x00" + original.ID
	key := identity + "\x00" + strconv.Itoa(tier)
	if held, ok := cache.Lookup(key); ok {
		return held, nil
	}
	derived, err := attachment.Derive(identity, original.Value.Data, original.Value.MimeType, maxWidth)
	if err != nil {
		return forgeattach.Entry{}, fmt.Errorf("forge attachment: %w", err)
	}
	if !derived.Derived {
		return original, nil
	}
	return cache.Put(key, forgeattach.Attachment{
		Data:     derived.Data,
		MimeType: derived.MimeType,
		Kind:     forgeattach.KindImage,
		Filename: original.Value.Filename,
		Width:    derived.Width,
		Height:   derived.Height,
	})
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
	return a.saveDownload(entry.Value.Filename, entry.Value.MimeType, entry.Value.Data)
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
	classified, err := forgeattach.Classify(data, filename)
	if err != nil {
		return forgeattach.Entry{}, err
	}
	return cache.Put(key, forgeattach.Attachment{
		Data:     data,
		MimeType: classified.MimeType,
		Kind:     classified.Kind,
		Filename: filename,
		Width:    classified.Width,
		Height:   classified.Height,
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
		MimeType: entry.Value.MimeType,
		Kind:     entry.Value.Kind,
		Filename: entry.Value.Filename,
		ModTime:  entry.StoredAt,
		Content:  nopCloserReader{bytes.NewReader(entry.Value.Data)},
	}, nil
}

// nopCloserReader adapts a retained byte slice onto the ReadSeekCloser
// the route streams. Nothing to release: the cache owns the bytes and
// outlives the response.
type nopCloserReader struct{ *bytes.Reader }

func (nopCloserReader) Close() error { return nil }
