// Package localimage presents an image file on this computer that rendered
// markdown references by path (`![x](/abs/shot.png)`). It owns the path gate,
// the checks that make the bytes safe to hand a browser, display-density
// derivatives, and the content ids the ticketed byte route serves them by.
package localimage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strconv"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/contentcache"
	"agent-overflow/internal/editor"
	"agent-overflow/internal/workspacefiles"
)

const (
	// CacheBytes bounds the derivatives held for the byte route. A 2160
	// tier of a 4K screenshot is a few MiB as PNG, so this holds every
	// derivative a few busy panes show, and still cannot grow past what a
	// desktop backend can spare for bytes that are cheap to make again.
	CacheBytes int64 = 192 << 20
	// CacheTTL is how long a resolved image stays servable. Longer than
	// the forge cache's because the pane that showed a local image
	// typically stays open; a miss after it costs one re-resolve.
	CacheTTL = 30 * time.Minute
	// originalEntryBytes is what an entry with no bytes of its own (an
	// original the route re-opens from disk) is charged against
	// CacheBytes, on top of its path, so a caller resolving many files
	// cannot grow the cache past its bound with weightless entries.
	originalEntryBytes = 256
)

// Image is one resolved local image, ready to mint a ticket for.
type Image struct {
	ContentID string
	MimeType  string
	// Width and Height are the served pixel size; zero when unknown.
	Width  int
	Height int
	// OriginalWidth and OriginalHeight are the file's pixel size; zero
	// when Go cannot read its header (svg, ico, avif).
	OriginalWidth  int
	OriginalHeight int
	OriginalBytes  int64
	// Derived is false when the served bytes are the file itself.
	Derived bool
}

// Content is one resolved image opened for the byte route, which closes it.
type Content struct {
	MimeType string
	// ModTime is the file's mtime for an original and when it was made
	// for a derivative.
	ModTime time.Time
	Content io.ReadSeekCloser
}

// Original is a file's validated bytes, for a caller that copies them.
type Original struct {
	Path     string
	Data     []byte
	MimeType string
}

// Service resolves local images and holds them for the byte route. Safe for
// concurrent use.
type Service struct {
	cache *contentcache.Cache[entry]
}

// source is the identity and facts of one version of a file: a changed
// size or mtime is a different source.
type source struct {
	path    string
	size    int64
	modTime time.Time
	mime    string
	width   int
	height  int
}

// entry is what one content id serves. data is nil for an original, which
// the route re-opens from disk rather than holding in memory.
type entry struct {
	source source
	data   []byte
	mime   string
	width  int
	height int
}

// New returns a service with an empty cache.
func New() *Service {
	return &Service{cache: contentcache.New(contentcache.Config[entry]{
		MaxBytes: CacheBytes,
		TTL:      CacheTTL,
		Size: func(e entry) int64 {
			return int64(len(e.data)) + int64(len(e.source.path)) + originalEntryBytes
		},
	})}
}

// Resolve gates and validates one image path and answers the content id that
// serves it at maxWidth device pixels: a derivative at the ladder tier for
// that width (attachment.Derive), or the file itself when maxWidth is not
// positive, exceeds the ladder, or no derivative applies.
//
// Every failure reads `load local image: <reason>: <cause>`; the reason is
// the short phrase a rendered chip shows beside the image's alt text.
func (s *Service) Resolve(path, workspacePath string, maxWidth int) (Image, error) {
	resolved, err := resolvePath(path, workspacePath)
	if err != nil {
		return Image{}, err
	}
	file, info, err := workspacefiles.OpenRegular(resolved)
	if err != nil {
		return Image{}, fileError(resolved, err)
	}
	defer closeLogged(file)

	identity := resolved + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	tier := attachment.DeriveTier(maxWidth)
	key := identity + "\x00" + strconv.Itoa(tier)
	if held, ok := s.cache.Lookup(key); ok {
		return answer(held), nil
	}

	src, data, err := readSource(file, info, resolved)
	if err != nil {
		return Image{}, err
	}
	served := entry{source: src, mime: src.mime, width: src.width, height: src.height}
	if tier > 0 {
		derived, err := attachment.Derive(identity, data, src.mime, maxWidth)
		if err != nil {
			return Image{}, decodeError(err)
		}
		if derived.Derived {
			served.data, served.mime = derived.Data, derived.MimeType
			served.width, served.height = derived.Width, derived.Height
		}
	}
	stored, err := s.cache.Put(key, served)
	if err != nil {
		return Image{}, localImageError("cannot hold image", err)
	}
	return answer(stored), nil
}

// Open serves one content id. An original is re-opened from disk and must
// still be the version Resolve validated; a changed size or mtime is an
// error, which the route answers 404, and the client resolves again.
func (s *Service) Open(contentID string) (Content, error) {
	held, ok := s.cache.Get(contentID)
	if !ok {
		return Content{}, fmt.Errorf("local image %q is no longer cached", contentID)
	}
	if held.Value.data != nil {
		return Content{
			MimeType: held.Value.mime,
			ModTime:  held.StoredAt,
			Content:  nopCloser{bytes.NewReader(held.Value.data)},
		}, nil
	}
	src := held.Value.source
	file, info, err := workspacefiles.OpenRegular(src.path)
	if err != nil {
		return Content{}, fmt.Errorf("open local image %s: %w", src.path, err)
	}
	if info.Size() != src.size || !info.ModTime().Equal(src.modTime) {
		closeErr := file.Close()
		return Content{}, errors.Join(fmt.Errorf("local image %s changed since it was resolved", src.path), closeErr)
	}
	return Content{MimeType: src.mime, ModTime: info.ModTime(), Content: file}, nil
}

// ReadOriginal gates, reads and validates one image file, with the same
// refusals Resolve gives.
func ReadOriginal(path, workspacePath string) (Original, error) {
	resolved, err := resolvePath(path, workspacePath)
	if err != nil {
		return Original{}, err
	}
	file, info, err := workspacefiles.OpenRegular(resolved)
	if err != nil {
		return Original{}, fileError(resolved, err)
	}
	defer closeLogged(file)
	src, data, err := readSource(file, info, resolved)
	if err != nil {
		return Original{}, err
	}
	return Original{Path: resolved, Data: data, MimeType: src.mime}, nil
}

// resolvePath applies the editor's path gate: canonical absolute paths, no
// network shares, relative paths only against a workspace.
func resolvePath(path, workspacePath string) (string, error) {
	resolved, err := editor.ResolvePath(path, workspacePath)
	if err != nil {
		return "", localImageError(fileReason(err, "path not allowed"), err)
	}
	return resolved, nil
}

// readSource reads an opened file under the display cap and proves it is an
// image a browser displays within the decode budget.
func readSource(file *os.File, info fs.FileInfo, resolved string) (source, []byte, error) {
	data, err := workspacefiles.ReadOpened(file, info, attachment.DisplayImageMaxBytes)
	if err != nil {
		return source{}, nil, fileError(resolved, err)
	}
	mime, err := attachment.DetectDisplayImageMIME(data)
	if err != nil {
		return source{}, nil, localImageError("not an image", err)
	}
	width, height, err := attachment.ValidateDisplayImage(data, mime)
	if err != nil {
		return source{}, nil, decodeError(err)
	}
	return source{
		path:    resolved,
		size:    info.Size(),
		modTime: info.ModTime(),
		mime:    mime,
		width:   width,
		height:  height,
	}, data, nil
}

func answer(held contentcache.Entry[entry]) Image {
	e := held.Value
	return Image{
		ContentID:      held.ID,
		MimeType:       e.mime,
		Width:          e.width,
		Height:         e.height,
		OriginalWidth:  e.source.width,
		OriginalHeight: e.source.height,
		OriginalBytes:  e.source.size,
		Derived:        e.data != nil,
	}
}

func localImageError(reason string, cause error) error {
	return fmt.Errorf("load local image: %s: %w", reason, cause)
}

func fileError(resolved string, err error) error {
	return localImageError(fileReason(err, "cannot read file"), fmt.Errorf("%s: %w", resolved, err))
}

func decodeError(err error) error {
	if errors.Is(err, attachment.ErrPixelBudget) {
		return localImageError("too many pixels", err)
	}
	return localImageError("cannot decode image", err)
}

// fileReason names a path-gate or read failure, or answers otherwise for one
// it has no phrase for. The os errors are matched by sentinel, so the phrase
// is the same on every platform whatever the OS spelled.
func fileReason(err error, otherwise string) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "file not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, workspacefiles.ErrNotRegularFile), errors.Is(err, editor.ErrNotRegularFile):
		return "not a file"
	case errors.Is(err, workspacefiles.ErrTooLarge):
		return fmt.Sprintf("larger than %d MiB", attachment.DisplayImageMaxBytes/(1024*1024))
	}
	return otherwise
}

// closeLogged releases a read-only descriptor whose bytes were already read
// in full. A close failure there cannot change what was read, so it is
// logged rather than turned into a failed load.
func closeLogged(file *os.File) {
	if err := file.Close(); err != nil {
		log.Printf("localimage: close %s: %v", file.Name(), err)
	}
}

// nopCloser adapts a held byte slice onto the ReadSeekCloser the route
// streams. Nothing to release: the cache owns the bytes.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
