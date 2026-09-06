package transferfiles

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
)

const (
	MaxFiles              = 16_384
	MaxFileBytes    int64 = 2 << 30
	MaxTotalBytes   int64 = 8 << 30
	MaxArchiveBytes int64 = MaxTotalBytes + MaxFiles*(16<<10)
)

// Source names one regular file under an injected root. Name is its portable
// archive path. Callers must quiesce writers before enumerating their snapshot.
type Source struct{ Root, Path, Name string }

// File is the verified content identity used to detect destination conflicts.
type File struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
}

// ValidName is deliberately portable: Windows drive names, alternate streams,
// device names, case aliases and separator spellings cannot escape admission
// just because the archive happened to be created on a Unix computer.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 4096 || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 || strings.TrimRight(part, " .") != part {
			return false
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
			(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
			return false
		}
		for _, c := range part {
			if c < 32 || strings.ContainsRune("<>\"|?*", c) {
				return false
			}
		}
	}
	return true
}

// Create writes a new archive, never replacing an existing operation snapshot.
// The hash covers the entire wire file, including headers and the end marker.
func Create(ctx context.Context, destination string, sources []Source) (digest string, err error) {
	if len(sources) > MaxFiles {
		return "", errors.New("transfer: too many files")
	}
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source.Root == "" || !fs.ValidPath(source.Path) || strings.ContainsAny(source.Path, "\\\x00") || !ValidName(source.Name) || seen[strings.ToLower(source.Name)] {
			return "", fmt.Errorf("transfer: invalid or duplicate file %q", source.Name)
		}
		seen[strings.ToLower(source.Name)] = true
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(out, hash))
	var total int64
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		size, err := addFile(ctx, tw, source, MaxTotalBytes-total)
		if err != nil {
			return "", err
		}
		total += size
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := atomicfile.SyncDir(filepath.Dir(destination)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func addFile(ctx context.Context, tw *tar.Writer, source Source, remaining int64) (int64, error) {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	// OpenRoot confines even a concurrently replaced path. Refuse links instead
	// of silently omitting part of a conversation or copying unrelated data.
	if err := regularPath(root, source.Path); err != nil {
		return 0, err
	}
	in, err := root.Open(filepath.FromSlash(source.Path))
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes || info.Size() > remaining {
		return 0, fmt.Errorf("transfer: file type or size is unsupported: %s", source.Name)
	}
	mode := int64(0o600)
	if info.Mode()&0o100 != 0 {
		mode = 0o700
	}
	if err := tw.WriteHeader(&tar.Header{Name: source.Name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return 0, err
	}
	if _, err := io.CopyN(tw, &contextReader{ctx, in}, info.Size()); err != nil {
		return 0, err
	}
	var extra [1]byte
	if n, err := in.Read(extra[:]); n != 0 || err != io.EOF {
		return 0, fmt.Errorf("transfer: file changed during snapshot: %s", source.Name)
	}
	after, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return 0, fmt.Errorf("transfer: file changed during snapshot: %s", source.Name)
	}
	return info.Size(), nil
}

func regularPath(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return fmt.Errorf("transfer: expected a regular file without symbolic links: %s", name)
		}
	}
	return nil
}

// Extract creates a private staging directory and removes it on ANY error.
// A successful return means every file and the complete archive hash verified;
// nothing is installed into a workspace or provider home by this function.
func Extract(ctx context.Context, input io.Reader, digest, destination string) (files []File, err error) {
	decoded, decodeErr := hex.DecodeString(digest)
	if decodeErr != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
		return nil, errors.New("transfer: invalid archive digest")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	hash := sha256.New()
	limited := &io.LimitedReader{R: &contextReader{ctx, input}, N: MaxArchiveBytes + 1}
	stream := io.TeeReader(limited, hash)
	tr := tar.NewReader(stream)
	seen := make(map[string]bool)
	var total int64
	for {
		before := limited.N
		h, err := tr.Next()
		if err == io.EOF {
			if before-limited.N < 1024 {
				return nil, errors.New("transfer: archive has no complete end marker")
			}
			break
		}
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(h.Name)
		if (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) || !ValidName(h.Name) || seen[key] || len(files) >= MaxFiles || h.Size < 0 || h.Size > MaxFileBytes || h.Size > MaxTotalBytes-total || h.Linkname != "" || len(h.PAXRecords) > 1 || h.PAXRecords["path"] != "" && h.PAXRecords["path"] != h.Name {
			return nil, fmt.Errorf("transfer: unsupported, duplicate or oversized archive member %q", h.Name)
		}
		for key := range h.PAXRecords {
			if key != "path" {
				return nil, fmt.Errorf("transfer: unsupported archive metadata %q", key)
			}
		}
		seen[key] = true
		file, err := extractFile(root, tr, h)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		total += h.Size
	}
	// Hash trailing bytes too, but permit only tar's zero padding. Concatenated
	// archives must not let another reader see members our validation skipped.
	buf := make([]byte, 32<<10)
	for {
		n, readErr := stream.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return nil, errors.New("transfer: unexpected data after archive")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if limited.N == 0 || (MaxArchiveBytes+1-limited.N)%512 != 0 || hex.EncodeToString(hash.Sum(nil)) != digest {
		return nil, errors.New("transfer: incomplete archive or content digest mismatch")
	}
	// Each file was synced; sync nested directories from the leaves up before
	// the coordinator can durably declare this snapshot prepared.
	if err := syncTree(destination); err != nil {
		return nil, err
	}
	if err := atomicfile.SyncDir(filepath.Dir(destination)); err != nil {
		return nil, err
	}
	return files, nil
}

func extractFile(root *os.Root, input io.Reader, h *tar.Header) (File, error) {
	if dir := path.Dir(h.Name); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
			return File{}, err
		}
	}
	mode := os.FileMode(0o600)
	if h.Mode&0o100 != 0 {
		mode = 0o700
	}
	out, err := root.OpenFile(filepath.FromSlash(h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return File{}, err
	}
	defer out.Close()
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(out, hash), input, h.Size); err != nil {
		return File{}, err
	}
	if err := out.Sync(); err != nil {
		return File{}, err
	}
	if err := out.Close(); err != nil {
		return File{}, err
	}
	return File{Name: h.Name, Size: h.Size, SHA256: hex.EncodeToString(hash.Sum(nil)), Executable: mode == 0o700}, nil
}

func syncTree(dir string) error {
	var dirs []string
	if err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, p)
		}
		return err
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicfile.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
