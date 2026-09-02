// Package bundle turns the SPA the backend already embeds into the two
// documents a phone shell needs to update itself: a SHA-256 manifest and
// a zip of exactly the files that manifest names.
//
// Why this exists (docs/specs/remote-access.md §9, "Bundle sync: the
// backend is the phone's update server"). Every other client of this
// backend is SERVED the bundle it then runs, so its web code and its
// wire can never skew. The APK is the one client that carries a bundle
// of its own, built whenever the store build was cut, and the answer
// ruled for it is that the backend it pairs with is its update server:
// no update SaaS, no release signing, and no second distribution
// channel. The paired session over pinned TLS is the integrity boundary,
// and the manifest served on that same session is what each file is
// checked against, so a bundle is exactly as trusted as the backend it
// came from — which is the trust the device already extended by pairing.
//
// This package is PURE. It reads an fs.FS and answers bytes; it knows
// nothing about HTTP, sessions, or phones. `internal/transport` owns the
// two routes and the credential in front of them.
//
// **What the walk costs, and when it is paid.** The manifest is built
// once, lazily, on the first caller that asks — which in practice is the
// first WebSocket connection, because the hello frame publishes the id.
// It hashes every served byte and retains nothing but the file list. The
// ARCHIVE is built on its own first caller, because that one retains a
// compressed copy of the whole bundle for the process's life and only a
// phone ever asks for it: a desktop backend with no shell paired must
// not carry ~5 MB and a deflate pass it will never serve. The two cannot
// disagree even so — the archive is built FROM the manifest's file list
// and re-verifies each file's digest as it compresses, so a file that no
// longer matches what the manifest published is an error here rather
// than a download every phone would reject after paying for it.
package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// MinShellBuild is the lowest Android `versionCode` this bundle's
// `native/` seams can run on.
//
// A bundle is web code, and web code that calls a Capacitor plugin the
// installed APK was never built with does not degrade — it answers null
// forever, or throws where nothing can catch it. So the bundle states
// the floor and the shell compares it against its own build before it
// downloads anything: a shell below the floor keeps the bundle it has
// and says, once, that the phone's app needs a store update. That is the
// single case in this design that gates on a version rather than on a
// capability, and it is here because the capability in question is
// NATIVE code that cannot be shipped over this channel at all.
//
// **Bump it when a seam needs a plugin an older APK lacks**, in the same
// change that adds the seam. Never for a web-only change: a bump costs
// every phone below it its updates until the person installs a new APK.
const MinShellBuild = 1

// IDFileName is where a built bundle records its own id, written into
// `frontend/dist` by the Vite plugin that mirrors the hash rule below
// (`frontend/scripts/bundleId.mjs`).
//
// It exists for exactly one reader: a shell running the bundle its APK
// was built with, which holds no state-file entry naming it and must
// still be able to say "the backend's id is the one I am already
// running". Every other case reads the id out of the native state file.
//
// It is EXCLUDED from the manifest, and that exclusion is what makes the
// two computations agree: the Vite plugin hashes `dist` and then writes
// this file into it, so a Go walk that counted it would hash a tree the
// plugin never saw.
const IDFileName = "bundle-id.txt"

// Included reports whether one path inside the dist tree is part of the
// bundle a phone runs.
//
// Two exclusions and no more, both of which must be mirrored exactly by
// `frontend/scripts/bundleId.mjs` (a fixture and one golden id pin them
// against each other):
//
//   - `*.map`. Source maps are emitted only by the perf-investigation
//     build (`AO_SOURCEMAP=1`) and no page ever requests one; shipping
//     them would put megabytes on a phone's link for a file nothing
//     loads, and it would make the id differ between two builds of
//     identical application code.
//   - IDFileName, for the reason its own comment gives.
//
// Directories are never offered here: the walk asks about files.
func Included(name string) bool {
	if name == IDFileName {
		return false
	}
	return !strings.HasSuffix(name, ".map")
}

// File is one served file, as the manifest publishes it.
type File struct {
	// Path is slash-separated and relative to the bundle root, exactly
	// as the page requests it and exactly as the zip entry is named.
	Path string `json:"path"`
	// SHA256 is the hex digest of the file's bytes. The shell checks
	// every unzipped file against it before the bundle is allowed to
	// become the next one.
	SHA256 string `json:"sha256"`
	// Size is the byte count, so a shell can refuse a truncated entry
	// before it has hashed it.
	Size int64 `json:"size"`
}

// Manifest is what a shell compares its running bundle against, and the
// document each downloaded file is verified with.
type Manifest struct {
	// ID identifies this bundle's CONTENT: the hex SHA-256 over the
	// sorted `path\x00sha256\n` lines of Files.
	//
	// Content rather than a version string, for two reasons that both
	// bite in practice. Two builds of identical content share an id, so
	// a phone paired with two machines running the same release
	// downloads nothing when it moves between them. And a "dev" build —
	// every build on a developer's box, since `main.version` defaults to
	// that — still gets a real, distinct id, so the update path can be
	// exercised without cutting a release.
	ID string `json:"id"`
	// Version is `main.version`, the link-time stamp. Display and
	// ordering only: the shell compares IDs to decide whether to
	// download, and reads this only to pick the NEWEST backend when
	// several are attached.
	Version string `json:"version"`
	// MinShellBuild is the constant above, carried so a shell can answer
	// the question without a second route.
	MinShellBuild int `json:"minShellBuild"`
	// Files is sorted by Path, which is what makes ID reproducible and
	// what lets a shell walk it against a staged directory in one pass.
	Files []File `json:"files"`
}

// Bundle is one embedded SPA, and the lazy cache in front of it.
//
// Safe for concurrent use: both documents are produced under a sync.Once
// and never mutated afterwards, so every reader after the first shares
// one immutable answer.
type Bundle struct {
	fsys    fs.FS
	version string

	manifestOnce sync.Once
	manifest     Manifest
	manifestErr  error

	archiveOnce sync.Once
	archive     []byte
	archiveErr  error
}

// New wraps the served file tree. `fsys` is the SPA root — the same
// `fs.Sub(assets, "frontend/dist")` the asset handler serves — and
// `version` is `main.version`.
//
// Nothing is read here. A backend that never has a client ask never
// walks the tree at all.
func New(fsys fs.FS, version string) *Bundle {
	return &Bundle{fsys: fsys, version: version}
}

// Manifest answers the file list and the content id, building it on the
// first call.
func (b *Bundle) Manifest() (Manifest, error) {
	if b == nil || b.fsys == nil {
		return Manifest{}, errors.New("bundle: no served file tree")
	}
	b.manifestOnce.Do(func() {
		b.manifest, b.manifestErr = b.build()
	})
	return b.manifest, b.manifestErr
}

// Archive answers the deflated zip of exactly the manifest's files,
// building it on the first call and retaining it afterwards.
//
// The returned slice is shared and MUST NOT be written to. Callers write
// it to a response and nothing else.
func (b *Bundle) Archive() ([]byte, error) {
	manifest, err := b.Manifest()
	if err != nil {
		return nil, err
	}
	b.archiveOnce.Do(func() {
		b.archive, b.archiveErr = b.compress(manifest)
	})
	return b.archive, b.archiveErr
}

// build walks the tree once and produces the manifest.
func (b *Bundle) build() (Manifest, error) {
	files := make([]File, 0, 64)
	digest := sha256.New()
	err := fs.WalkDir(b.fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !Included(name) {
			return nil
		}
		sum, size, err := b.hashFile(name, digest)
		if err != nil {
			return err
		}
		files = append(files, File{Path: name, SHA256: sum, Size: size})
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("bundle: walk served files: %w", err)
	}
	if len(files) == 0 {
		// An empty tree is a build that did not happen, not a bundle
		// with nothing in it. Publishing one would let a phone stage an
		// empty directory over a working app.
		return Manifest{}, errors.New("bundle: the served file tree is empty")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Manifest{
		ID:            ID(files),
		Version:       b.version,
		MinShellBuild: MinShellBuild,
		Files:         files,
	}, nil
}

// ID is the content-id rule, in one place because two implementations
// have to agree on it (`frontend/scripts/bundleId.mjs`).
//
// The hash covers `path\x00sha256\n` per file over files sorted by path.
// NUL is the separator because it is the one byte a path cannot contain,
// so no path can spell a field boundary; the newline terminates each
// record so a digest cannot be shifted by a path that ends where the
// next one begins. Nothing else is covered: not sizes (implied by the
// digest), not the version (two builds of identical content are the same
// bundle), and not modification times, which are not content.
func ID(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	digest := sha256.New()
	for _, file := range sorted {
		_, _ = digest.Write([]byte(file.Path))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(file.SHA256))
		_, _ = digest.Write([]byte{'\n'})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// hashFile streams one file through the shared hasher. Streaming rather
// than fs.ReadFile: the largest entry in a production dist is a few MB,
// and there is no reason for the manifest walk's peak to be the size of
// the biggest chunk.
func (b *Bundle) hashFile(name string, digest hash.Hash) (string, int64, error) {
	file, err := b.fsys.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest.Reset()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

// compress builds the zip from the MANIFEST's file list rather than from
// a second walk, and re-verifies each digest as it goes.
//
// That is what makes the two documents unable to disagree: the archive
// holds exactly the entries the manifest names, in the manifest's order,
// and a file whose bytes no longer hash to what was published stops here
// rather than reaching a phone.
func (b *Bundle) compress(manifest Manifest) ([]byte, error) {
	// Deflate lands a production dist at roughly a third of its size, so
	// reserving half the served bytes takes the growth path out of the
	// common case without over-reserving.
	total := int64(0)
	for _, file := range manifest.Files {
		total += file.Size
	}
	buf := bytes.NewBuffer(make([]byte, 0, total/2))
	archive := zip.NewWriter(buf)
	digest := sha256.New()
	for _, file := range manifest.Files {
		// No Modified: a zero time is written identically on every
		// build, so two builds of identical content produce identical
		// archive bytes — the same property the id has.
		entry, err := archive.CreateHeader(&zip.FileHeader{Name: file.Path, Method: zip.Deflate})
		if err != nil {
			return nil, fmt.Errorf("bundle: archive %s: %w", file.Path, err)
		}
		sum, size, err := b.copyVerified(file.Path, entry, digest)
		if err != nil {
			return nil, fmt.Errorf("bundle: archive %s: %w", file.Path, err)
		}
		if sum != file.SHA256 || size != file.Size {
			return nil, fmt.Errorf("bundle: %s changed under the manifest", file.Path)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("bundle: finish archive: %w", err)
	}
	return buf.Bytes(), nil
}

// copyVerified writes one file into the archive and hashes it on the way
// through, so verification costs one read rather than two.
func (b *Bundle) copyVerified(name string, dst io.Writer, digest hash.Hash) (string, int64, error) {
	file, err := b.fsys.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest.Reset()
	size, err := io.Copy(io.MultiWriter(dst, digest), file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

// CleanPath reports whether a manifest path is one a shell may write to
// disk under a bundle directory. It lives here, beside the writer of the
// document, rather than only in the consumer that enforces it.
//
// It refuses an absolute path, any `..` or `.` element, a backslash or a
// drive letter, and any path that does not survive path.Clean unchanged.
// The Android plugin re-checks the same rule on every entry it unzips,
// which is the arrangement intended: the producer states it and the
// consumer enforces it, so neither has to trust the other.
func CleanPath(name string) bool {
	switch {
	case name == "", strings.HasPrefix(name, "/"), strings.Contains(name, `\`), strings.Contains(name, ":"):
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, element := range strings.Split(name, "/") {
		if element == "" || element == "." || element == ".." {
			return false
		}
	}
	return true
}
