// Package localimage presents an image file on this computer that rendered
// markdown references by path (`![x](/abs/shot.png)`). It owns the path gate,
// the checks that make the bytes safe to hand a browser, display-density
// derivatives and the disk cache that keeps them across restarts, and the
// content ids the ticketed byte route serves them by.
package localimage

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/contentcache"
	"agent-overflow/internal/editor"
	"agent-overflow/internal/workspacefiles"
)

const (
	// CacheBytes bounds the resolved images held in memory for the byte
	// route. An entry is metadata, its paths and sizes, except a derivative
	// the disk store could not take, which is held as bytes until CacheTTL.
	// The bound is sized for that fallback: a 2160 tier of a 4K screenshot
	// is a few MiB as PNG, so it holds every derivative a few busy panes
	// show, and still cannot grow past what a desktop backend can spare.
	CacheBytes int64 = 192 << 20
	// CacheTTL is how long a resolved image stays servable by its content
	// id. Longer than the forge cache's because the pane that showed a
	// local image typically stays open. A resolve after it costs opening
	// the source and two header reads when the derivative is on disk, and a
	// derivation when it is not.
	CacheTTL = 30 * time.Minute
	// entryBytes is what every entry is charged against CacheBytes on top
	// of its paths and held bytes, so a caller resolving many files cannot
	// grow the cache past its bound with near-weightless entries.
	entryBytes = 256
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
	// ModTime is the file's mtime for an original and when it was
	// resolved for a derivative.
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
	cache   *contentcache.Cache[entry]
	disk    *diskStore
	reports *reports
	// derive is attachment.Derive; a test hook that counts derivations.
	derive func(key string, src []byte, mime string, maxWidth int) (attachment.Derived, error)
}

// source is the identity and facts of one version of a file: a changed
// size or mtime is a different source.
type source struct {
	path    string
	size    int64
	modTime time.Time
	width   int
	height  int
}

// entry is what one content id serves: the file itself (source.path) when
// neither file nor data is set, a derivative in the disk store (file, under
// fileKey), or a derivative the disk store could not take, held as data.
type entry struct {
	source  source
	file    string
	fileKey diskKey
	data    []byte
	mime    string
	width   int
	height  int
}

// New returns a service with an empty memory cache that keeps derivatives
// in dir, bounded by DiskCacheBytes. When dir is empty or cannot be opened,
// every derivative is held in memory for CacheTTL instead, and the first
// one logs why.
func New(dir string) *Service {
	return newService(dir, DiskCacheBytes, log.Printf)
}

func newService(dir string, diskBytes int64, logf func(format string, args ...any)) *Service {
	reports := newReports(logf)
	return &Service{
		cache: contentcache.New(contentcache.Config[entry]{
			MaxBytes: CacheBytes,
			TTL:      CacheTTL,
			Size: func(e entry) int64 {
				return int64(len(e.source.path)) + int64(len(e.file)) + int64(len(e.data)) + entryBytes
			},
		}),
		disk:    openDiskStore(dir, diskBytes, reports),
		reports: reports,
		derive:  attachment.Derive,
	}
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
		if held.Value.file == "" {
			return answer(held), nil
		}
		if _, stored := s.disk.use(held.Value.fileKey); stored {
			return answer(held), nil
		}
		// The disk store evicted or discarded the file this entry names.
		s.cache.Remove(held.ID)
	}

	var fileKey diskKey
	if tier > 0 {
		fileKey = diskKeyFor(key)
		if reused, ok := s.reuse(file, info, resolved, fileKey); ok {
			return s.hold(key, reused)
		}
	}
	src, mime, data, err := readSource(file, info, resolved)
	if err != nil {
		return Image{}, err
	}
	served := entry{source: src, mime: mime, width: src.width, height: src.height}
	if tier > 0 {
		derived, err := s.derive(identity, data, mime, maxWidth)
		if err != nil {
			return Image{}, decodeError(err)
		}
		if derived.Derived {
			served.mime, served.width, served.height = derived.MimeType, derived.Width, derived.Height
			if path, err := s.disk.put(fileKey, derived.MimeType, derived.Data); err != nil {
				s.reports.report("store derivative, holding it in memory", err)
				served.data = derived.Data
			} else {
				served.file, served.fileKey = path, fileKey
			}
		}
	}
	return s.hold(key, served)
}

// reuse answers the derivative the disk store holds under key without
// deriving: the served size from the file's header, the original's from the
// source's. The source was validated when the file was written, and key
// names that version of it. Any failure answers false and the caller
// derives again; a stored file that cannot be read is discarded.
func (s *Service) reuse(sourceFile *os.File, info fs.FileInfo, resolved string, key diskKey) (entry, bool) {
	path, ok := s.disk.use(key)
	if !ok {
		return entry{}, false
	}
	mime, served, err := readDerivativeHeader(path)
	if err != nil {
		s.reports.report("reuse derivative", err)
		s.disk.discard(key)
		return entry{}, false
	}
	// ReadAt leaves the file offset where readSource expects it if this
	// fails and the caller falls through.
	original, _, err := image.DecodeConfig(io.NewSectionReader(sourceFile, 0, info.Size()))
	if err != nil {
		return entry{}, false
	}
	s.disk.touch(path)
	return entry{
		source: source{
			path:    resolved,
			size:    info.Size(),
			modTime: info.ModTime(),
			width:   original.Width,
			height:  original.Height,
		},
		file:    path,
		fileKey: key,
		mime:    mime,
		width:   served.Width,
		height:  served.Height,
	}, true
}

// readDerivativeHeader reads a stored derivative's header and checks the
// format it names against the type its extension claims.
func readDerivativeHeader(path string) (string, image.Config, error) {
	file, _, err := workspacefiles.OpenRegular(path)
	if err != nil {
		return "", image.Config{}, err
	}
	defer closeLogged(file)
	cfg, format, err := image.DecodeConfig(file)
	if err != nil {
		return "", image.Config{}, fmt.Errorf("read derivative %s: %w", path, err)
	}
	mime := "image/" + format
	if diskExtensions[mime] != filepath.Ext(path) {
		return "", image.Config{}, fmt.Errorf("derivative %s holds %s: %w", path, format, errWrongFormat)
	}
	return mime, cfg, nil
}

func (s *Service) hold(key string, served entry) (Image, error) {
	stored, err := s.cache.Put(key, served)
	if err != nil {
		return Image{}, localImageError("cannot hold image", err)
	}
	return answer(stored), nil
}

// Open serves one content id. An original is re-opened from disk and must
// still be the version Resolve validated; a changed size or mtime is an
// error, which the route answers 404; the next Resolve of the path reads
// the new version. A derivative is opened from the disk store; one evicted
// or deleted since it was resolved is an error too, and the next Resolve
// derives it again.
func (s *Service) Open(contentID string) (Content, error) {
	held, ok := s.cache.Get(contentID)
	if !ok {
		return Content{}, fmt.Errorf("local image %q is no longer cached", contentID)
	}
	e := held.Value
	if e.data != nil {
		return Content{
			MimeType: e.mime,
			ModTime:  held.StoredAt,
			Content:  nopCloser{bytes.NewReader(e.data)},
		}, nil
	}
	if e.file != "" {
		file, _, err := workspacefiles.OpenRegular(e.file)
		if err != nil {
			s.disk.discard(e.fileKey)
			return Content{}, fmt.Errorf("open local image derivative %s: %w", e.file, err)
		}
		return Content{MimeType: e.mime, ModTime: held.StoredAt, Content: file}, nil
	}
	src := e.source
	file, info, err := workspacefiles.OpenRegular(src.path)
	if err != nil {
		return Content{}, fmt.Errorf("open local image %s: %w", src.path, err)
	}
	if info.Size() != src.size || !info.ModTime().Equal(src.modTime) {
		closeErr := file.Close()
		return Content{}, errors.Join(fmt.Errorf("local image %s changed since it was resolved", src.path), closeErr)
	}
	return Content{MimeType: e.mime, ModTime: info.ModTime(), Content: file}, nil
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
	_, mime, data, err := readSource(file, info, resolved)
	if err != nil {
		return Original{}, err
	}
	return Original{Path: resolved, Data: data, MimeType: mime}, nil
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
// image a browser displays within the decode budget. It answers the source,
// its sniffed type and its bytes.
func readSource(file *os.File, info fs.FileInfo, resolved string) (source, string, []byte, error) {
	data, err := workspacefiles.ReadOpened(file, info, attachment.DisplayImageMaxBytes)
	if err != nil {
		return source{}, "", nil, fileError(resolved, err)
	}
	mime, err := attachment.DetectDisplayImageMIME(data)
	if err != nil {
		return source{}, "", nil, localImageError("not an image", err)
	}
	width, height, err := attachment.ValidateDisplayImage(data, mime)
	if err != nil {
		return source{}, "", nil, decodeError(err)
	}
	return source{
		path:    resolved,
		size:    info.Size(),
		modTime: info.ModTime(),
		width:   width,
		height:  height,
	}, mime, data, nil
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
		Derived:        e.file != "" || e.data != nil,
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

// closeLogged releases a read-only descriptor after the reads that needed
// it. A close failure there cannot change what was read, so it is logged
// rather than turned into a failed load.
func closeLogged(file *os.File) {
	if err := file.Close(); err != nil {
		log.Printf("localimage: close %s: %v", file.Name(), err)
	}
}

// nopCloser adapts a held byte slice onto the ReadSeekCloser the route
// streams. Nothing to release: the cache owns the bytes.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
