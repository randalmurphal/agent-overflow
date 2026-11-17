package browser

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// A pane download on a desktop engine ends up where any browser on this
// machine puts it: the user's Downloads folder (downloadSaving). The hosted
// engine's WebView2 saves there itself. The engines in this process write
// into AO's own directory, which nothing else touches, and the Manager
// publishes each finished file here (publishDownload), so an engine's
// writes, cancel and failure cleanup only ever reach the download's own
// file.

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

// maxDownloadNameCollisions bounds the " (N)" walk.
const maxDownloadNameCollisions = 1000

// linkDownload is os.Link, a test seam for a Downloads folder on another
// filesystem.
var linkDownload = os.Link

// publishDownload moves a finished download from AO's staging directory
// into the user's Downloads folder under the first free name of the
// suggested one and its " (1)", " (2)" variants, as browsers name them,
// without ever replacing a file: it hard-links the staged file to the
// candidate name, taking the next on EEXIST, so the published file is the
// staged inode whose identity it answers. Across filesystems (EXDEV) it
// creates the candidate exclusively, copies through that descriptor, and
// answers the descriptor's identity. The staged name is removed whatever
// happens, and a failed copy removes its file only while it is still the
// one it created, so nothing partial stays in Downloads.
func publishDownload(staged, suggested string) (path string, file os.FileInfo, err error) {
	defer func() {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			log.Printf("browser: remove the staged download %s: %v", staged, removeErr)
		}
	}()
	source, info, err := openStagedDownload(staged)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = source.Close() }()
	dir, err := userDownloadsDir()
	if err != nil {
		return "", nil, err
	}
	name := safeArtifactName(suggested, "download")
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	copying := false
	for i := 0; i < maxDownloadNameCollisions; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		if copying {
			file, taken, err := copyDownload(source, info, candidate)
			if err != nil {
				return "", nil, fmt.Errorf("browser: save in the Downloads folder: %w", err)
			}
			if taken {
				return candidate, file, nil
			}
			continue
		}
		err := linkDownload(staged, candidate)
		switch {
		case err == nil:
			linked, err := os.Lstat(candidate)
			if err != nil {
				return "", nil, fmt.Errorf("browser: save in the Downloads folder: %w", err)
			}
			if !os.SameFile(linked, info) {
				return "", nil, fmt.Errorf("browser: %s changed while the download was saved there", candidate)
			}
			return candidate, linked, nil
		case errors.Is(err, fs.ErrExist):
		case errors.Is(err, syscall.EXDEV):
			copying = true
			i--
		default:
			return "", nil, fmt.Errorf("browser: save in the Downloads folder: %w", err)
		}
	}
	return "", nil, fmt.Errorf("browser: %d files named like %q are already in %s", maxDownloadNameCollisions, name, dir)
}

// openStagedDownload opens the staged file, which must be a regular file and
// not a link to one.
func openStagedDownload(staged string) (*os.File, os.FileInfo, error) {
	linked, err := os.Lstat(staged)
	if err != nil {
		return nil, nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	if !linked.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("browser: the download's file %s is not a regular file", staged)
	}
	source, err := os.Open(staged)
	if err != nil {
		return nil, nil, fmt.Errorf("browser: the downloaded file: %w", err)
	}
	info, err := source.Stat()
	if err == nil && !os.SameFile(info, linked) {
		err = fmt.Errorf("browser: the download's file %s changed as it was opened", staged)
	}
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}
	return source, info, nil
}

// copyDownload creates path exclusively and copies source into it. taken
// is false when something is already at path. A failed copy removes the
// file it created, and only that file.
func copyDownload(source *os.File, info os.FileInfo, path string) (file os.FileInfo, taken bool, err error) {
	target, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if errors.Is(err, fs.ErrExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	created, err := target.Stat()
	if err != nil {
		_ = target.Close()
		return nil, true, err
	}
	if _, err = source.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(target, source)
	}
	if err == nil {
		file, err = target.Stat()
	}
	if closeErr := target.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if removeErr := removeOwnFile(path, created); removeErr != nil {
			err = fmt.Errorf("%w; %v", err, removeErr)
		}
		return nil, true, err
	}
	return file, true, nil
}

// removeOwnFile removes path while it is still created, the file AO made
// there; a file someone put in its place stays.
func removeOwnFile(path string, created os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("browser: check the partial copy %s: %w", path, err)
	}
	if !os.SameFile(current, created) {
		return fmt.Errorf("browser: the partial copy %s was replaced and is left there", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("browser: remove the partial copy %s: %w", path, err)
	}
	return nil
}
