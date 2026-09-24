package app

import (
	"bytes"
	"errors"
	"fmt"

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
	return a.saveDownload(entry.Filename, entry.MimeType, entry.Data)
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
