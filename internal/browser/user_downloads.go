package browser

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	defer func() { _ = file.Close() }()
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

// maxDownloadNameCollisions bounds the walk over a name's variants.
const maxDownloadNameCollisions = 1000

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
// is ever copied and nothing partial ever has a public name. The caller's release of the staged file
// (releaseStaged) removes a linked file's staged name. Where the filesystem
// has neither, the download fails with the reason.
func publishDownload(staged, suggested string) (path string, file os.FileInfo, err error) {
	info, downloads, err := stagedFileOf(staged)
	if err != nil {
		return "", nil, err
	}
	name := safeArtifactName(suggested, "download")
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return placeNoReplace(staged, info, downloads, "the Downloads folder", func(i int) string {
		if i == 0 {
			return name
		}
		return fmt.Sprintf("%s (%d)%s", stem, i, ext)
	})
}

// placeNoReplace gives the regular file source, whose Lstat is info, the
// first name in dir that nobody holds, of name(0), name(1), ..., and
// answers its path and file. It never replaces a file: it hard-links
// source to a candidate, or, on a filesystem that refuses hard links,
// renames it with a rename that refuses to replace; either way a name that
// is taken (EEXIST) moves on to the next, so two placements at once never
// take one name. A link leaves source's own name for the caller to remove.
// where names dir in an error.
func placeNoReplace(source string, info os.FileInfo, dir, where string, name func(i int) string) (path string, file os.FileInfo, err error) {
	move := linkDownload
	linking := true
	for i := 0; i < maxDownloadNameCollisions; i++ {
		candidate := filepath.Join(dir, name(i))
		err := move(source, candidate)
		switch {
		case err == nil:
			moved, err := os.Lstat(candidate)
			if err != nil {
				return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
			}
			if !os.SameFile(moved, info) {
				return "", nil, fmt.Errorf("browser: %s changed while the download was saved there", candidate)
			}
			return candidate, moved, nil
		case errors.Is(err, fs.ErrExist):
		case linking && linksRefused(err):
			move, linking = renameDownload, false
			i--
		case !linking && noReplaceRenameRefused(err):
			return "", nil, fmt.Errorf("browser: %s %s supports neither hard links nor renames that refuse to replace a file, so the download is not saved there: %w", where, dir, err)
		default:
			return "", nil, fmt.Errorf("browser: save in %s: %w", where, err)
		}
	}
	return "", nil, fmt.Errorf("browser: %d files named like %q are already in %s", maxDownloadNameCollisions, name(0), dir)
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
