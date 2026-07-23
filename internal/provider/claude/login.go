package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/externalurl"
)

const defaultLoginTimeout = 10 * time.Minute

type LoginConfig struct {
	Binary            string
	ConfigDir         string
	BrowserExecutable string
	Timeout           time.Duration
}

// Login runs Claude Code's native OAuth command in an isolated
// CLAUDE_CONFIG_DIR. Claude owns the browser callback and credential write;
// Agent Overflow only supplies a browser executable so WSL can route the URL
// to Windows.
func Login(ctx context.Context, cfg LoginConfig) error {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "claude"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}
	loginCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(loginCtx, binary, "auth", "login", "--claudeai")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+cfg.ConfigDir)
	if cfg.BrowserExecutable != "" {
		cmd.Env = append(
			cmd.Env,
			"BROWSER="+cfg.BrowserExecutable,
			externalurl.BrowserHelperEnvironment+"="+externalurl.BrowserHelperValue,
		)
	}
	if err := cmd.Run(); err != nil {
		if loginCtx.Err() != nil {
			return fmt.Errorf("claude: login: %w", loginCtx.Err())
		}
		// Provider output may contain OAuth state, PKCE values, or future
		// credential fields. Keep it out of RPC/UI errors entirely.
		return fmt.Errorf("claude: login command failed: %w", err)
	}
	return nil
}
