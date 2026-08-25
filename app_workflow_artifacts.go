package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/safecopy"
	"agent-overflow/internal/workflowhost"
)

// WorkflowArtifact is one app-managed file deliverable copied from a phase
// workspace. Files are discovered from the deterministic per-item directory.
type WorkflowArtifact struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

func (a *App) workflowDataRoot() string {
	if root := a.workflowRunner.DataRoot(); strings.TrimSpace(root) != "" {
		return root
	}
	return a.configDir
}

func listWorkflowArtifacts(dataRoot, itemID string) (result []WorkflowArtifact, resultErr error) {
	if filepath.Base(itemID) != itemID || itemID == "." || itemID == ".." {
		return nil, fmt.Errorf("invalid work item id")
	}
	directory := workflowhost.ArtifactDir(dataRoot, itemID)
	if err := safecopy.ValidateDestination(dataRoot, filepath.Join(directory, ".artifact")); err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	artifactRoot, err := workflowhost.OpenArtifactRoot(dataRoot, itemID)
	if errors.Is(err, fs.ErrNotExist) {
		return []WorkflowArtifact{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, artifactRoot.Close()) }()
	entries, err := fs.ReadDir(artifactRoot.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("list workflow artifacts: %w", err)
	}
	byName := make(map[string]WorkflowArtifact, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), safecopy.TempPrefix) {
			continue
		}
		info, err := fs.Stat(artifactRoot.FS(), entry.Name())
		if err != nil {
			return nil, fmt.Errorf("inspect workflow artifact %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		artifact := WorkflowArtifact{
			Name: strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			Path: filepath.Join(directory, entry.Name()), Size: info.Size(), ModTime: info.ModTime().UnixMilli(),
		}
		if current, exists := byName[artifact.Name]; !exists || artifact.ModTime > current.ModTime || artifact.ModTime == current.ModTime && artifact.Path > current.Path {
			byName[artifact.Name] = artifact
		}
	}
	artifacts := make([]WorkflowArtifact, 0, len(byName))
	for _, artifact := range byName {
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts, nil
}
