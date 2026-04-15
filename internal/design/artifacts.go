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

func (as *ArtifactStore) writeHTML(path, html string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		return fmt.Errorf("write artifact html: %w", err)
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
