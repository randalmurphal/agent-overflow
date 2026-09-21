package codex

import (
	"fmt"
	"os"

	"agent-overflow/internal/provider"
)

// prepareAccountProcess gives account operations an empty configuration scope.
// A temporary CODEX_HOME makes ~/.codex eligible as project configuration when
// cwd is ~. The empty directory and disabled ancestor search prevent that second
// load while preserving user configuration in the selected CODEX_HOME.
// The caller removes the directory after the process has closed.
func prepareAccountProcess(cfg provider.SpawnConfig) (provider.SpawnConfig, func() error, error) {
	if err := provider.ValidateProbeWorkDir("codex account", cfg.Dir); err != nil {
		return cfg, nil, err
	}
	dir, err := os.MkdirTemp(cfg.Dir, ".ao-codex-account-")
	if err != nil {
		return cfg, nil, fmt.Errorf("codex: create account working directory: %w", err)
	}
	cfg.Dir = dir
	cfg.Provider = "codex"
	cfg.Args = append([]string{"-c", "project_root_markers=[]"}, cfg.Args...)
	return cfg, func() error {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("codex: remove account working directory: %w", err)
		}
		return nil
	}, nil
}
