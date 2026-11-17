package browser

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// A pane download on a desktop engine goes where any browser on this
// machine puts it: the user's Downloads folder (artifactDownloadEngine says
// which engines differ). The hosted engine's WebView2 resolves the Windows
// folder itself; the engines in this process resolve it here, per download,
// and name the file so it overwrites nothing (reserveDownloadPath).

// userDownloadsDir is the Downloads folder of the user this process runs
// as. On Linux it is XDG_DOWNLOAD_DIR from $XDG_CONFIG_HOME/user-dirs.dirs
// ($HOME/.config when unset), else $HOME/Downloads; on macOS it is
// $HOME/Downloads. The folder must already exist: a browser download never
// creates the user's folders, and a missing one is the download's error.
func userDownloadsDir() (string, error) {
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
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("browser: the Downloads folder %s does not exist", dir)
		}
		return "", fmt.Errorf("browser: the Downloads folder %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("browser: the Downloads folder %s is not a folder", dir)
	}
	return dir, nil
}

// xdgDownloadDir reads XDG_DOWNLOAD_DIR from the user-dirs file, "" when the
// file or the entry is absent. The file's values are `"$HOME/..."` or an
// absolute path (xdg-user-dirs).
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
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || key != "XDG_DOWNLOAD_DIR" {
			continue
		}
		value, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("browser: the user folders name XDG_DOWNLOAD_DIR as %s, which is not a quoted path", scanner.Text())
		}
		if rest, ok := strings.CutPrefix(value, "$HOME"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			value = home + rest
		}
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("browser: the user folders name XDG_DOWNLOAD_DIR as %q, which is not an absolute path", value)
		}
		return filepath.Clean(value), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("browser: read the user folders: %w", err)
	}
	return "", nil
}

// maxDownloadNameCollisions bounds the " (N)" walk.
const maxDownloadNameCollisions = 1000

// downloadPaths are the Downloads paths this process gave to downloads
// still running. A name is free only when nothing is at it and no running
// download holds it: two downloads of one name must not both be given it.
var downloadPaths = struct {
	sync.Mutex
	held map[string]bool
}{held: make(map[string]bool)}

// The two ways an engine takes a Downloads path (reserveDownloadPath).
const (
	// claimDownloadFile creates the file, empty and exclusively, so it is
	// the download's before the engine writes it: an engine that deletes
	// its destination when the download fails (WebKitGTK does, a file it
	// did not create included) can then only delete the download's own.
	claimDownloadFile = true
	// nameDownloadFile only names a free path, for an engine that refuses
	// a destination that exists (WKWebView); a file another program puts
	// there before the engine writes fails the download.
	nameDownloadFile = false
)

// reserveDownloadPath gives a download a path in the user's Downloads
// folder that named nothing: the sanitized suggested name, or that name
// with " (1)", " (2)" and on before its extension, as browsers do. claim
// is claimDownloadFile or nameDownloadFile. The caller releases the path
// (releaseDownloadPath) when the download ends, and removes a claimed file
// the engine never took.
func reserveDownloadPath(suggested string, claim bool) (string, error) {
	dir, err := userDownloadsDir()
	if err != nil {
		return "", err
	}
	name := safeArtifactName(suggested, "download")
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	downloadPaths.Lock()
	defer downloadPaths.Unlock()
	for i := 0; i < maxDownloadNameCollisions; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		if downloadPaths.held[candidate] {
			continue
		}
		if hook := downloadPathChecked; hook != nil {
			hook(candidate)
		}
		taken, err := takeDownloadPath(candidate, claim)
		if err != nil {
			return "", fmt.Errorf("browser: save in the Downloads folder %s: %w", dir, err)
		}
		if !taken {
			continue
		}
		downloadPaths.held[candidate] = true
		return candidate, nil
	}
	return "", fmt.Errorf("browser: %d files named like %q are already in %s", maxDownloadNameCollisions, name, dir)
}

// takeDownloadPath answers whether path is free, creating its file when
// claim is set; a file already there is not an error.
func takeDownloadPath(path string, claim bool) (bool, error) {
	if !claim {
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

// downloadPathChecked is a test seam run just before a free-looking path is
// taken, where another program could put a file there first. Guarded by
// downloadPaths; nil outside tests.
var downloadPathChecked func(path string)

// releaseDownloadPath lets go of a download's path once the download
// ended: its file is there, finished, or the engine removed it.
func releaseDownloadPath(path string) {
	downloadPaths.Lock()
	delete(downloadPaths.held, path)
	downloadPaths.Unlock()
}
