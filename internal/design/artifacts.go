package design

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ArtifactStore persists HTML artifacts on disk and their metadata in SQLite.
type ArtifactStore struct {
	baseDir string
	store   *store.Store
}

// NewArtifactStore constructs a design artifact store rooted at baseDir.
func NewArtifactStore(baseDir string, st *store.Store) *ArtifactStore {
	return &ArtifactStore{
		baseDir: strings.TrimSpace(baseDir),
		store:   st,
	}
}

// Store writes the HTML artifact to disk and persists its metadata.
func (as *ArtifactStore) Store(threadID, html, title, description, kind string) (DesignArtifact, error) {
	if as.store == nil {
		return DesignArtifact{}, fmt.Errorf("artifact store unavailable")
	}

	threadID = strings.TrimSpace(threadID)
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	kind = strings.TrimSpace(kind)

	if threadID == "" {
		return DesignArtifact{}, fmt.Errorf("thread ID is required")
	}
	if title == "" {
		return DesignArtifact{}, fmt.Errorf("title is required")
	}
	if html == "" {
		return DesignArtifact{}, fmt.Errorf("html is required")
	}
	if kind == "" {
		kind = "render"
	}

	artifact := DesignArtifact{
		ID:          uuid.New().String(),
		ThreadID:    threadID,
		Title:       title,
		Description: description,
		Kind:        kind,
		CreatedAt:   time.Now().UnixMilli(),
	}
	artifactPath, err := as.artifactPath(threadID, artifact.ID)
	if err != nil {
		return DesignArtifact{}, err
	}
	artifact.HTMLPath = artifactPath

	if err := as.writeHTML(artifact.HTMLPath, html); err != nil {
		return DesignArtifact{}, err
	}
	if err := as.store.InsertDesignArtifact(artifact); err != nil {
		_ = os.Remove(artifact.HTMLPath)
		return DesignArtifact{}, err
	}
	return artifact, nil
}

// Get loads the stored HTML content for a thread artifact.
func (as *ArtifactStore) Get(threadID, artifactID string) (string, error) {
	if as.store == nil {
		return "", fmt.Errorf("artifact store unavailable")
	}

	threadID = strings.TrimSpace(threadID)
	artifactID = strings.TrimSpace(artifactID)

	if _, err := as.store.GetDesignArtifact(threadID, artifactID); err != nil {
		return "", err
	}

	artifactPath, err := as.artifactPath(threadID, artifactID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return "", fmt.Errorf("read design artifact %s: %w", artifactID, err)
	}
	return string(data), nil
}

// List returns design artifacts for a thread, optionally filtered by kind.
func (as *ArtifactStore) List(threadID, kind string) ([]DesignArtifact, error) {
	if as.store == nil {
		return nil, fmt.Errorf("artifact store unavailable")
	}
	return as.store.ListDesignArtifacts(strings.TrimSpace(threadID), strings.TrimSpace(kind))
}

// SavePNG writes a PNG sibling to the artifact's HTML on disk. The frontend
// captures HTML→PNG via captureHtmlToPng once a design:artifact event lands,
// then uploads the bytes here so future "Send to chat" handoffs (Bundle /
// PNG-only) can attach the rendered image without re-running the capture.
//
// SavePNG verifies the artifact metadata exists before writing — that pins
// the file path inside the artifact's namespace and rejects writes for
// missing or other-thread artifacts.
func (as *ArtifactStore) SavePNG(threadID, artifactID string, png []byte) error {
	if as.store == nil {
		return fmt.Errorf("artifact store unavailable")
	}
	if len(png) == 0 {
		return fmt.Errorf("png payload is empty")
	}
	threadID = strings.TrimSpace(threadID)
	artifactID = strings.TrimSpace(artifactID)
	if _, err := as.store.GetDesignArtifact(threadID, artifactID); err != nil {
		return err
	}

	pngPath, err := as.pngPath(threadID, artifactID)
	if err != nil {
		return err
	}
	return as.writeBytes(pngPath, png)
}

// GetPNGPath returns the on-disk path of an artifact's PNG capture, or empty
// string if no PNG has been saved yet. Returns error only if the artifact
// metadata cannot be loaded.
func (as *ArtifactStore) GetPNGPath(threadID, artifactID string) (string, error) {
	if as.store == nil {
		return "", fmt.Errorf("artifact store unavailable")
	}
	threadID = strings.TrimSpace(threadID)
	artifactID = strings.TrimSpace(artifactID)
	if _, err := as.store.GetDesignArtifact(threadID, artifactID); err != nil {
		return "", err
	}

	pngPath, err := as.pngPath(threadID, artifactID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(pngPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat artifact png: %w", err)
	}
	return pngPath, nil
}

// GetPNG reads an artifact's stored PNG bytes. Returns (nil, nil) when no
// PNG has been written for the artifact (e.g. capture failed or hasn't run
// yet); the caller distinguishes "absent" from a hard read error.
func (as *ArtifactStore) GetPNG(threadID, artifactID string) ([]byte, error) {
	pngPath, err := as.GetPNGPath(threadID, artifactID)
	if err != nil {
		return nil, err
	}
	if pngPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(pngPath)
	if err != nil {
		return nil, fmt.Errorf("read artifact png: %w", err)
	}
	return data, nil
}

func (as *ArtifactStore) writeHTML(path, html string) error {
	return as.writeBytes(path, []byte(html))
}

func (as *ArtifactStore) writeBytes(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}

	// Atomic write: write to temp file, sync, rename. Prevents partial reads
	// if the process crashes mid-write.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create artifact temp file: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write artifact bytes: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync artifact bytes: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close artifact temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename artifact bytes: %w", err)
	}
	return nil
}

func (as *ArtifactStore) artifactPath(threadID, artifactID string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	artifactID = strings.TrimSpace(artifactID)
	if threadID == "" {
		return "", fmt.Errorf("thread ID is required")
	}
	if artifactID == "" {
		return "", fmt.Errorf("artifact ID is required")
	}
	return as.resolveWithinBase(threadID, fmt.Sprintf("%s.html", artifactID))
}

func (as *ArtifactStore) pngPath(threadID, artifactID string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	artifactID = strings.TrimSpace(artifactID)
	if threadID == "" {
		return "", fmt.Errorf("thread ID is required")
	}
	if artifactID == "" {
		return "", fmt.Errorf("artifact ID is required")
	}
	return as.resolveWithinBase(threadID, fmt.Sprintf("%s.png", artifactID))
}

func (as *ArtifactStore) resolveWithinBase(parts ...string) (string, error) {
	baseDir := strings.TrimSpace(as.baseDir)
	if baseDir == "" {
		return "", fmt.Errorf("artifact base directory is required")
	}

	basePath, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve artifact base directory: %w", err)
	}

	targetPath, err := filepath.Abs(filepath.Join(append([]string{basePath}, parts...)...))
	if err != nil {
		return "", fmt.Errorf("resolve artifact path: %w", err)
	}
	if !pathWithinBase(basePath, targetPath) {
		return "", fmt.Errorf("artifact path escapes base directory")
	}
	return targetPath, nil
}

func pathWithinBase(basePath, targetPath string) bool {
	basePath = filepath.Clean(basePath)
	targetPath = filepath.Clean(targetPath)
	return targetPath == basePath || strings.HasPrefix(targetPath, basePath+string(os.PathSeparator))
}
