package localimage

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/appdirs"
)

// DiskCacheBytes bounds the derivative files kept on disk. A 2160 tier of a
// 4K screenshot is a few MiB as PNG and the lower tiers are well under one,
// so this keeps the tiers of several hundred screenshots across restarts
// while staying small beside the database and attachments in the same data
// directory.
const DiskCacheBytes int64 = 256 << 20

const (
	// diskBlock is the unit a file is charged against the bound in: what a
	// filesystem allocates, so the total tracks disk use, and a floor that
	// caps the index at maxBytes/diskBlock records however small the files.
	diskBlock = 4 << 10
	// tempPrefix starts the name a derivative is written under before it is
	// renamed into place. The scan removes such files: they are writes a
	// previous process never finished.
	tempPrefix = ".tmp-"
	// derivativeFormat names what attachment.Derive produces and is hashed
	// into every file name. Bump it when Derive changes its output for the
	// same source and tier, so files the previous derivation wrote are never
	// found again and age out of the bound instead of being served.
	derivativeFormat = 1
)

// diskExtensions maps the types attachment.Derive encodes to the extension
// a stored derivative carries; the extension is how a reuse knows the type.
var diskExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
}

var (
	errNoCacheDir      = errors.New("no derivative cache directory")
	errUnstorableType  = errors.New("derivative type has no cache extension")
	errLargerThanBound = errors.New("derivative is larger than the cache bound")
	errWrongFormat     = errors.New("derivative format does not match its extension")
)

// diskKey names one derivative: the sha256 of the derivation format, the
// source identity (resolved path, size, mtime) and the tier.
type diskKey [sha256.Size]byte

func diskKeyFor(sourceKey string) diskKey {
	return sha256.Sum256([]byte(strconv.Itoa(derivativeFormat) + "\x00" + sourceKey))
}

// diskFile is one indexed derivative. charge is its size rounded up to
// diskBlock.
type diskFile struct {
	key    diskKey
	ext    string
	charge int64
}

// diskStore keeps derivatives as files in one directory, named by diskKey,
// bounded by maxBytes with least-recently-used eviction. The index is the
// authority after the startup scan: the scan records every file's size and
// orders them by mtime, and from then on writes, uses and evictions update
// the index and the directory together.
//
// A use moves a file to the front of the index. Its mtime is set only when
// it is written and when a resolve reuses it from disk, which is how the
// next process's scan sees the order: the memory cache answers repeat
// resolves for CacheTTL, so the mtime of a file in constant use lags its
// last use by at most that.
type diskStore struct {
	dir      string
	maxBytes int64
	reports  *reports
	// rename moves a finished temp file into place; a test hook.
	rename func(oldpath, newpath string) error
	// err is why the directory could not be opened; put fails with it, and
	// the caller holds its derivatives in memory instead.
	err error

	mu    sync.Mutex
	order *list.List // front = most recently used; values are *diskFile
	index map[diskKey]*list.Element
	total int64
}

// openDiskStore opens dir, creating it owner-only, and scans it. A failure
// is kept as the store's err rather than returned: the service still serves
// every derivative from memory, and put reports the cause.
func openDiskStore(dir string, maxBytes int64, reports *reports) *diskStore {
	s := &diskStore{
		dir:      dir,
		maxBytes: maxBytes,
		reports:  reports,
		rename:   os.Rename,
		order:    list.New(),
		index:    make(map[diskKey]*list.Element),
	}
	if err := s.scan(); err != nil {
		s.err = fmt.Errorf("open derivative cache %q: %w", dir, err)
	}
	return s
}

func (s *diskStore) scan() error {
	if s.dir == "" {
		return errNoCacheDir
	}
	if !filepath.IsAbs(s.dir) {
		return errors.New("derivative cache directory is not absolute")
	}
	if err := os.MkdirAll(s.dir, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	type found struct {
		file    *diskFile
		modTime time.Time
	}
	var files []found
	unknown := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, tempPrefix) {
			s.remove("remove unfinished derivative", filepath.Join(s.dir, name))
			continue
		}
		key, ext, ok := parseDiskName(name)
		if !ok || !entry.Type().IsRegular() {
			unknown++
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.reports.report("scan derivative cache", err)
			}
			continue
		}
		files = append(files, found{
			file:    &diskFile{key: key, ext: ext, charge: chargeFor(info.Size())},
			modTime: info.ModTime(),
		})
	}
	if unknown > 0 {
		s.reports.logf("localimage: leaving %d unrecognized entries in %s", unknown, s.dir)
	}
	// The store is not shared until openDiskStore returns, so the index is
	// built and bounded here without the lock.
	slices.SortFunc(files, func(a, b found) int { return a.modTime.Compare(b.modTime) })
	for _, f := range files {
		// One key under two extensions means Derive changed its output
		// type without a derivativeFormat bump; the newer file wins.
		if older, ok := s.index[f.file.key]; ok {
			s.dropLocked(older)
		}
		s.index[f.file.key] = s.order.PushFront(f.file)
		s.total += f.file.charge
	}
	s.evictLocked()
	return nil
}

// use answers the path of the file stored under key and marks it the most
// recently used.
func (s *diskStore) use(key diskKey) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.index[key]
	if !ok {
		return "", false
	}
	s.order.MoveToFront(element)
	return s.pathOf(element.Value.(*diskFile)), true
}

// touch sets a reused file's mtime, which is what the next scan orders by.
func (s *diskStore) touch(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.reports.report("touch derivative", err)
	}
}

// put stores data as the derivative for key and answers its path. The bytes
// go to a temp file in the directory, synced, and are renamed into place,
// so the final name never holds a partial file. A file already under the
// name is replaced. Storing may evict the least recently used files.
func (s *diskStore) put(key diskKey, mime string, data []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	ext, ok := diskExtensions[mime]
	if !ok {
		return "", fmt.Errorf("%w: %s", errUnstorableType, mime)
	}
	charge := chargeFor(int64(len(data)))
	if charge > s.maxBytes {
		return "", errLargerThanBound
	}
	temp, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return "", err
	}
	if err := writeSynced(temp, data); err != nil {
		s.remove("remove unfinished derivative", temp.Name())
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	file := &diskFile{key: key, ext: ext, charge: charge}
	final := s.pathOf(file)
	if err := s.rename(temp.Name(), final); err != nil {
		s.remove("remove unfinished derivative", temp.Name())
		return "", err
	}
	if element, ok := s.index[key]; ok {
		// A concurrent resolve of the same source stored it first; the
		// rename replaced that file with the same bytes.
		s.total -= element.Value.(*diskFile).charge
		s.order.Remove(element)
	}
	s.index[key] = s.order.PushFront(file)
	s.total += charge
	s.evictLocked()
	return final, nil
}

// discard drops the file stored under key, from the index and the disk. A
// caller discards a file it could not open or read; the next resolve of the
// source derives and stores it again.
func (s *diskStore) discard(key diskKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.index[key]; ok {
		s.dropLocked(element)
	}
}

func (s *diskStore) evictLocked() {
	for s.total > s.maxBytes {
		oldest := s.order.Back()
		if oldest == nil {
			return
		}
		s.dropLocked(oldest)
	}
}

// dropLocked removes one file from the index and the directory. A failed
// delete is reported and the record still goes: keeping it would make the
// bound loop on a file it cannot free.
func (s *diskStore) dropLocked(element *list.Element) {
	file := element.Value.(*diskFile)
	s.order.Remove(element)
	if s.index[file.key] == element {
		delete(s.index, file.key)
	}
	s.total -= file.charge
	s.remove("remove derivative", s.pathOf(file))
}

func (s *diskStore) remove(op, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.reports.report(op, err)
	}
}

func (s *diskStore) pathOf(file *diskFile) string {
	return filepath.Join(s.dir, hex.EncodeToString(file.key[:])+file.ext)
}

// bytes reports the charged total of the indexed files. Tests only.
func (s *diskStore) bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// parseDiskName reads a stored derivative's name: the lowercase hex key and
// one of diskExtensions. Anything else is not the store's.
func parseDiskName(name string) (diskKey, string, bool) {
	ext := filepath.Ext(name)
	if !slices.Contains(slices.Collect(maps.Values(diskExtensions)), ext) {
		return diskKey{}, "", false
	}
	stem := strings.TrimSuffix(name, ext)
	var key diskKey
	decoded, err := hex.DecodeString(stem)
	if err != nil || len(decoded) != len(key) || hex.EncodeToString(decoded) != stem {
		return diskKey{}, "", false
	}
	copy(key[:], decoded)
	return key, ext, true
}

func chargeFor(size int64) int64 {
	if size <= 0 {
		return diskBlock
	}
	return (size + diskBlock - 1) / diskBlock * diskBlock
}

// writeSynced writes data, syncs it and closes the file. The sync is what
// keeps a crash after the rename from leaving a truncated file under the
// final name.
func writeSynced(file *os.File, data []byte) error {
	_, err := file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// reports logs each distinct failure cause once. A cause is the operation
// and the innermost wrapped error, which leaves out the path a
// *fs.PathError carries, so one failure hitting every file logs one line.
type reports struct {
	logf func(format string, args ...any)
	mu   sync.Mutex
	seen map[string]bool
}

func newReports(logf func(format string, args ...any)) *reports {
	return &reports{logf: logf, seen: make(map[string]bool)}
}

func (r *reports) report(op string, err error) {
	cause := op + ": " + innermost(err).Error()
	r.mu.Lock()
	logged := r.seen[cause]
	r.seen[cause] = true
	r.mu.Unlock()
	if !logged {
		r.logf("localimage: %s: %v", op, err)
	}
}

func innermost(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
