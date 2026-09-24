package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"agent-overflow/internal/wsldistro"
)

// downloadsDir is where a file saved on this computer lands: the user's
// own Downloads folder, because that is where a person looks for a file
// they just saved.
//
// Under the Windows launcher the backend runs in WSL, whose home is not
// where the Windows user looks, so the launcher's exported Windows
// Downloads folder wins when it resolves (wsldistro.WindowsDownloadsDir).
// The app-private fallback exists for a headless or minimal home with no
// Downloads folder and for an isolated boot (downloadsIsolated), and is
// created rather than assumed.
func (a *App) downloadsDir() (string, error) {
	if !a.downloadsIsolated {
		if dir, ok := wsldistro.WindowsDownloadsDir(); ok {
			return dir, nil
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, "Downloads")
			if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
				return candidate, nil
			}
		}
	}
	if a.configDir == "" {
		return "", errors.New("app data directory is not initialised")
	}
	dir := filepath.Join(a.configDir, "downloads")
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}
	return dir, nil
}

// saveDownload writes data under name into downloadsDir without replacing
// an existing file, and returns the path it wrote. mimeType is the type
// the bytes were classified as; it supplies the extension a name without
// one needs to open in the right program.
func (a *App) saveDownload(name, mimeType string, data []byte) (string, error) {
	dir, err := a.downloadsDir()
	if err != nil {
		return "", err
	}
	return writeWithoutOverwriting(dir, downloadFileName(name, mimeType), data)
}

// maxDownloadCollisions bounds the " (2)" walk. A directory already
// holding this many copies of one name is a caller in a loop, and
// answering an error is better than the walk becoming the cost.
const maxDownloadCollisions = 200

// writeWithoutOverwriting creates the file exclusively, so a name already
// in Downloads is never clobbered: the suffix walk is the same " (2)"
// convention a browser uses, and O_EXCL is what makes the check and the
// create one operation rather than a race.
func writeWithoutOverwriting(dir, name string, data []byte) (string, error) {
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for attempt := 1; attempt <= maxDownloadCollisions; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, attempt, extension)
		}
		path := filepath.Join(dir, candidate)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("save file: %w", err)
		}
		if _, err := file.Write(data); err != nil {
			return "", discardPartialDownload(file, path, err)
		}
		if err := file.Close(); err != nil {
			return "", discardPartialDownload(nil, path, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("save file: %d files named like %q already exist", maxDownloadCollisions, name)
}

// discardPartialDownload removes a file whose write failed, so a failed
// save leaves nothing behind that looks like a finished one. A removal
// that also fails is reported with the write error rather than dropped.
func discardPartialDownload(file *os.File, path string, cause error) error {
	if file != nil {
		// The write already failed; the close only releases the handle.
		_ = file.Close()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("save file: %w (and the partial file %s could not be removed: %v)", cause, path, err)
	}
	return fmt.Errorf("save file: %w", cause)
}

const (
	downloadStemMaxRunes      = 80
	downloadExtensionMaxRunes = 16
)

// downloadFileName makes one user-supplied filename safe to create in a
// directory the user browses.
//
// Unlike sanitizeCIFileSegment this keeps Unicode letters and the
// extension: the name came from the person who uploaded the file, and a
// screenshot called "Скриншот.png" should still be that file after a
// save rather than a row of dashes. What it removes is everything that
// decides where a file goes or how a shell reads it.
//
// A name with no extension gets the one mimeType calls for. A GitHub asset
// reference names its bytes by an opaque id alone, and without an
// extension the saved file opens in no program. By the time a save runs
// the bytes have been classified, so the extension states what they are
// rather than guessing from the reference.
func downloadFileName(name, mimeType string) string {
	name = strings.TrimSpace(name)
	// Both separators, not the platform's: the name may come from another
	// machine, so a Windows-shaped one has to be cut on Linux too.
	name = name[strings.LastIndexAny(name, `/\`)+1:]

	// The separating dot is re-added rather than sanitized through: a
	// leading dot is exactly what the segment cleaner strips, and losing
	// it would turn every save into an extensionless file.
	raw := filepath.Ext(name)
	extension := sanitizeDownloadSegment(strings.TrimPrefix(raw, "."), downloadExtensionMaxRunes)
	if extension != "" {
		extension = "." + extension
	} else {
		extension = downloadExtensions[strings.ToLower(strings.TrimSpace(mimeType))]
	}
	stem := sanitizeDownloadSegment(strings.TrimSuffix(name, raw), downloadStemMaxRunes)
	if stem == "" {
		stem = "attachment"
	}
	return stem + extension
}

// downloadExtensions maps every media type the byte classifiers answer
// (attachment.DetectDisplayImageMIME, forgeattach.Classify) to the
// extension a saved file of that type carries. A fixed table rather than
// mime.ExtensionsByType, whose answer depends on the host's mime database
// and can be ".jpe" for a JPEG. A type absent here (a `file` attachment's
// display type) adds nothing: its name already carries the extension its
// type was read from.
var downloadExtensions = map[string]string{
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/avif":               ".avif",
	"image/bmp":                ".bmp",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
	"image/tiff":               ".tiff",
	"image/svg+xml":            ".svg",
	"video/mp4":                ".mp4",
	"video/quicktime":          ".mov",
	"video/webm":               ".webm",
	"video/avi":                ".avi",
	"video/ogg":                ".ogv",
	"audio/mp4":                ".m4a",
	"audio/mpeg":               ".mp3",
	"audio/wav":                ".wav",
	"audio/flac":               ".flac",
	"audio/aac":                ".aac",
	"audio/ogg":                ".ogg",
}

func sanitizeDownloadSegment(segment string, maxRunes int) string {
	var b strings.Builder
	count := 0
	for _, r := range segment {
		if count >= maxRunes {
			break
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r),
			r == '.', r == '-', r == '_', r == ' ', r == '(', r == ')':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		count++
	}
	// A leading dot would hide the file; a trailing one is invalid on
	// Windows. Spaces at either end survive no filesystem usefully.
	return strings.Trim(b.String(), " .-")
}
