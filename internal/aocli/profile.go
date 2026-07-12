package aocli

import (
	"fmt"
	"path/filepath"

	"agent-overflow/internal/project"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

func loadProjectBindings(configRoot, projectSlug string) (def.Bindings, error) {
	if projectSlug == "" {
		return nil, nil
	}
	root, err := resolveConfigRoot(configRoot)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(project.ConfigDir(root, projectSlug), "profile.yaml")
	loaded, defaulted, err := profile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load project profile %q: %w", path, err)
	}
	if defaulted {
		return nil, nil
	}
	return &loaded, nil
}
