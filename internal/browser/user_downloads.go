package browser

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
)

// A pane download on a desktop engine ends up where any browser on this
// machine puts it: the user's Downloads folder (downloadSaving). The hosted
// engine's WebView2 saves there itself. The engines in this process write
// into the app's private staging directory there (download_staging.go),
// and the Manager publishes each finished file beside it
// (publishDownload), so an engine's writes, cancel and failure cleanup
// only ever reach the download's own staged file.

// userDownloadsDir is the Downloads folder of the user this process runs
// as, created when it does not exist, as browsers do. On Linux it is
// XDG_DOWNLOAD_DIR from $XDG_CONFIG_HOME/user-dirs.dirs ($HOME/.config when
// unset), else $HOME/Downloads; on macOS it is $HOME/Downloads. It answers
// the folder with its links resolved, the form authorizeFile compares. A
// path that is not a folder, or a folder that cannot be created, is the
// error.
func userDownloadsDir() (string, error) {
	dir, err := userDownloadsPath()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("browser: the Downloads folder %s: %w", dir, err)
	}
	return resolved, nil
}

// userDownloadsPath is userDownloadsDir before its links are resolved.
func userDownloadsPath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("browser: no Downloads folder: HOME is not set")
	}
	dir := filepath.Join(home, "Downloads")
	if runtime.GOOS == "linux" {
		configured, err := xdgDownloadDir(home)
		if err != nil {
			return "", err
		}
		if configured != "" {
			dir = configured
		}
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("browser: create the Downloads folder: %w", err)
		}
		return dir, nil
	}
	if err != nil {
		return "", fmt.Errorf("browser: the Downloads folder %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("browser: the Downloads folder %s is not a folder", dir)
	}
	return dir, nil
}

// xdgDownloadDir reads XDG_DOWNLOAD_DIR from the user-dirs file, "" when the
// file or the entry is absent; the last entry wins, as when a shell sources
// the file. The file is read, never run.
func xdgDownloadDir(home string) (string, error) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	file, err := os.Open(filepath.Join(config, "user-dirs.dirs"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("browser: read the user folders: %w", err)
	}
	defer func() {
		// Read-only: what was read stands, so a failed close is logged.
		if err := file.Close(); err != nil {
			log.Printf("browser: close the user folders file %s: %v", file.Name(), err)
		}
	}()
	found := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || strings.TrimSpace(key) != "XDG_DOWNLOAD_DIR" {
			continue
		}
		dir, err := userDirsValue(strings.TrimSpace(value), home)
		if err != nil {
			return "", fmt.Errorf("browser: the user folders name XDG_DOWNLOAD_DIR as %s: %w", strings.TrimSpace(value), err)
		}
		found = dir
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("browser: read the user folders: %w", err)
	}
	return found, nil
}

// userDirsValue reads one user-dirs.dirs value as xdg-user-dirs writes it:
// double-quoted, a backslash taking the next character as it is, and a
// leading $HOME/ (or exactly $HOME) standing for home. Any other value is
// the error.
func userDirsValue(value, home string) (string, error) {
	inner, ok := strings.CutPrefix(value, `"`)
	if !ok {
		return "", errors.New("the value is not in double quotes")
	}
	var b strings.Builder
	closed := false
	rest := ""
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\\' && i+1 < len(inner) {
			i++
			b.WriteByte(inner[i])
			continue
		}
		if c == '"' {
			closed, rest = true, inner[i+1:]
			break
		}
		b.WriteByte(c)
	}
	if !closed {
		return "", errors.New("the closing quote is missing")
	}
	if trailing := strings.TrimSpace(rest); trailing != "" && !strings.HasPrefix(trailing, "#") {
		return "", errors.New("text follows the closing quote")
	}
	path := b.String()
	// $HOME is the one expansion: in the raw value, so an escaped \$HOME is
	// a literal name.
	if strings.HasPrefix(inner, "$HOME") {
		after := path[len("$HOME"):]
		if after != "" && !strings.HasPrefix(after, "/") {
			return "", errors.New("$HOME is followed by more than a path")
		}
		path = home + after
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("the path is not absolute")
	}
	return filepath.Clean(path), nil
}

// maxNameVariants bounds every walk over a name's variants for one that is
// free in a directory: a download's or an artifact's name, a private name,
// a run directory.
const maxNameVariants = 1000

// linkDownload is os.Link and renameDownload renameNoReplace: test seams
// for a file appearing at a name as it is taken, and for a filesystem
// without hard links.
var (
	linkDownload   = os.Link
	renameDownload = renameNoReplace
)

// errNoReplaceRename is renameNoReplace's answer where the platform has no
// rename that refuses to replace.
var errNoReplaceRename = errors.New("this platform has no rename that refuses to replace a file")

// publishDownload moves a finished download from its staged file into the
// Downloads folder its staging directory is in, under the first free name
// of the suggested one and its " (1)", " (2)" variants, as browsers name
// them, and answers the published path and file. It never replaces a file
// (placeNoReplace). Both names stay within one directory tree, so nothing
// is ever copied and nothing partial ever has a public name. The caller's
// release of the staged file (releaseStaged) removes a linked file's
// staged name. Where the filesystem has neither hard links nor a rename
// that refuses to replace, the download fails with the reason.
func publishDownload(staged, suggested string) (path string, file os.FileInfo, err error) {
	source, downloads, err := stagedFileOf(staged)
	if err != nil {
		return "", nil, err
	}
	name := safeArtifactName(suggested, "download")
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return placeNoReplace(source, downloads, "the Downloads folder", func(i int) string {
		if i == 0 {
			return name
		}
		return fmt.Sprintf("%s (%d)%s", stem, i, ext)
	})
}

// placeSource is a regular file to place (placeNoReplace): its path, its
// identity as checked where it is held, and its directory, held open,
// where its private name is made and removed.
type placeSource struct {
	path string
	info os.FileInfo
	dir  *os.Root
}

// placeNoReplace gives the regular file source the first name in dir that
// nobody holds, of name(0), name(1), ..., and answers its path and
// source's identity, the one identity the caller trusts: never what a name
// answers afterwards. The file takes a private name beside source first
// (placingName), where the recovery of source's owner removes it if the
// placement never finishes, and only once that name is checked, in the
// held directory, to hold source's file does it take a public one. A file
// swapped in at source meanwhile is refused with nothing public made, and
// its private name undone. The public name is checked to hold source's
// file too: one that holds anything else, swapped in at it or reached
// through a directory moved meanwhile, fails the placement and stays as it
// is, since it may not be AO's. It never replaces a file: it hard-links,
// or, on a filesystem that refuses hard links, renames with a rename that
// refuses to replace; either way a name that is taken (EEXIST) moves on to
// the next, so two placements at once never take one name. A link leaves
// source's own name for the caller to remove. where names dir in an error.
func placeNoReplace(source placeSource, dir, where string, name func(i int) string) (path string, file os.FileInfo, err error) {
	place := placement{source: source, move: linkDownload, linking: true, where: where}
	private, err := place.private()
	if err != nil {
		return "", nil, err
	}
	for i := 0; i < maxNameVariants; i++ {
		candidate := filepath.Join(dir, name(i))
		err := place.move(private, candidate)
		switch {
		case err == nil:
			placed, err := os.Lstat(candidate)
			place.dropPrivate(private)
			if err == nil && os.SameFile(placed, source.info) {
				return candidate, source.info, nil
			}
			if err != nil {
				return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
			}
			return "", nil, fmt.Errorf("browser: %s changed as the download was saved there, so the download is not saved", candidate)
		case errors.Is(err, fs.ErrExist):
		default:
			place.undo(private)
			return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
		}
	}
	place.undo(private)
	return "", nil, fmt.Errorf("browser: %d files named like %q are already in %s", maxNameVariants, name(0), dir)
}

// placingNamePrefix starts the private name a file takes beside its source
// before its public one (placeNoReplace).
const placingNamePrefix = ".agent-overflow-placing-"

// placingNamePattern matches a private name placingName makes.
var placingNamePattern = regexp.MustCompile(`^` + regexp.QuoteMeta(placingNamePrefix) + `[0-9a-f]{16}$`)

// placingName is a fresh private name.
func placingName() string {
	var b [8]byte
	// crypto/rand.Read never answers an error: it ends the process.
	_, _ = rand.Read(b[:])
	return placingNamePrefix + hex.EncodeToString(b[:])
}

// placement is one placeNoReplace of source: how it moves the file (a hard
// link, or a no-replace rename where links are refused).
type placement struct {
	source  placeSource
	move    func(from, to string) error
	linking bool
	where   string
}

// private gives the source a private name beside it and answers its path,
// once the held directory shows that name holding the source's file. A
// refused link switches the placement to renames.
func (p *placement) private() (string, error) {
	for range maxNameVariants {
		name := placingName()
		private := filepath.Join(filepath.Dir(p.source.path), name)
		err := p.move(p.source.path, private)
		switch {
		case err == nil:
			held, err := p.source.dir.Lstat(name)
			if err == nil && os.SameFile(held, p.source.info) {
				return private, nil
			}
			p.undo(private)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("browser: save in %s: %w", p.where, err)
			}
			return "", fmt.Errorf("browser: %s changed before the download was saved, so it is not saved", p.source.path)
		case errors.Is(err, fs.ErrExist):
		case p.linking && linksRefused(err):
			p.move, p.linking = renameDownload, false
		case !p.linking && noReplaceRenameRefused(err):
			return "", fmt.Errorf("browser: %s %s supports neither hard links nor renames that refuse to replace a file, so the download is not saved there: %w", p.where, filepath.Dir(p.source.path), err)
		default:
			return "", fmt.Errorf("browser: save in %s: %w", p.where, err)
		}
	}
	return "", fmt.Errorf("browser: no private name is free beside %s", p.source.path)
}

// dropPrivate removes, through the held directory, the private name a link
// left once the file has its public one; a rename left none.
func (p *placement) dropPrivate(private string) {
	if !p.linking {
		return
	}
	if err := p.source.dir.Remove(filepath.Base(private)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the private name %s: %v", private, err)
	}
}

// undo ends a placement that gives the file no public name: a link's
// private name goes, and a rename's file goes back to source.
func (p *placement) undo(private string) {
	if p.linking {
		p.dropPrivate(private)
		return
	}
	if err := renameDownload(private, p.source.path); err != nil {
		log.Printf("browser: return %s to %s: %v", private, p.source.path, err)
	}
}

// linksRefused reports whether a link failed because the filesystem does
// not make hard links.
func linksRefused(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// noReplaceRenameRefused reports whether a no-replace rename failed because
// the filesystem or platform does not offer it.
func noReplaceRenameRefused(err error) bool {
	return errors.Is(err, errNoReplaceRename) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}
