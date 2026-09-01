package serviceinstall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type systemdManager struct {
	config Config
	runner Runner
}

func (m *systemdManager) Name() string { return "systemd (user)" }

func (m *systemdManager) unitName() string { return ServiceName + ".service" }

func (m *systemdManager) UnitPath() string {
	return filepath.Join(m.config.configHome(), "systemd", "user", m.unitName())
}

// UnitContents generates the unit file.
//
// Restart=on-failure, not always: a clean exit is the operator stopping the
// backend, and a supervisor that restarts one of those cannot be stopped.
// A saved listen port that will not bind exits non-zero, which IS a failure
// and is worth retrying — the port may be held by something on its way out.
//
// RestartSec keeps a genuinely unbindable port from becoming a busy loop, and
// StartLimit* let systemd give up and say so rather than retry forever.
func (m *systemdManager) UnitContents() (string, error) {
	args := make([]string, 0, len(m.config.serveArgs()))
	for _, arg := range m.config.serveArgs() {
		quoted, err := systemdQuote(arg)
		if err != nil {
			return "", err
		}
		args = append(args, quoted)
	}
	return `[Unit]
Description=Agent Overflow backend
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=120
StartLimitBurst=5

[Service]
Type=simple
ExecStart=` + strings.Join(args, " ") + `
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, nil
}

func (m *systemdManager) Install(ctx context.Context) error {
	contents, err := m.UnitContents()
	if err != nil {
		return err
	}
	path := m.UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("serviceinstall: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("serviceinstall: write %s: %w", path, err)
	}
	if err := m.mustRun(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return m.mustRun(ctx, "systemctl", "--user", "enable", "--now", m.unitName())
}

func (m *systemdManager) Uninstall(ctx context.Context) error {
	// Not mustRun: a unit that was never loaded, or a systemd that already
	// forgot it, is the state uninstall is trying to reach. Removing the file
	// still has to happen.
	_, _, err := m.runner.Run(ctx, "systemctl", "--user", "disable", "--now", m.unitName())
	if err != nil {
		return err
	}
	if err := os.Remove(m.UnitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("serviceinstall: remove %s: %w", m.UnitPath(), err)
	}
	return m.mustRun(ctx, "systemctl", "--user", "daemon-reload")
}

func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	status := Status{Manager: m.Name(), UnitPath: m.UnitPath()}
	if _, err := os.Stat(status.UnitPath); err == nil {
		status.Installed = true
	} else if !os.IsNotExist(err) {
		return status, fmt.Errorf("serviceinstall: stat %s: %w", status.UnitPath, err)
	}

	enabled, _, err := m.runner.Run(ctx, "systemctl", "--user", "is-enabled", m.unitName())
	if err != nil {
		return status, err
	}
	status.Enabled = enabled == "enabled"

	active, _, err := m.runner.Run(ctx, "systemctl", "--user", "is-active", m.unitName())
	if err != nil {
		return status, err
	}
	status.Running = active == "active"
	status.Detail = active

	return status, nil
}

func (m *systemdManager) Notes() []string {
	return []string{
		"A user service stops when your last session on this machine ends, and starts " +
			"again when you log in. For a backend that stays up across reboots with nobody " +
			"logged in, enable lingering: loginctl enable-linger $USER",
		"Logs: journalctl --user -u " + m.unitName() + " -f",
	}
}

// mustRun treats a non-zero exit as a failure, for the commands where it is
// one. It quotes the manager rather than paraphrasing it: systemd's own
// message names the real problem far better than a wrapper can.
func (m *systemdManager) mustRun(ctx context.Context, name string, args ...string) error {
	output, code, err := m.runner.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("serviceinstall: %s: %w", name, err)
	}
	if code != 0 {
		return fmt.Errorf("serviceinstall: %s %s exited %d%s",
			name, strings.Join(args, " "), code, indentedOutput(output))
	}
	return nil
}

func indentedOutput(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	return ":\n  " + strings.ReplaceAll(strings.TrimSpace(output), "\n", "\n  ")
}

// systemdQuote renders one argv element for an ExecStart line.
//
// Two escapes matter and neither is optional. `%` introduces a systemd
// specifier, so a literal one is `%%` — a home directory with a percent in it
// would otherwise expand to something else entirely. Whitespace separates
// arguments, so a path containing a space must be quoted, and inside those
// quotes `"` and `\` are escaped. A newline cannot be represented in a unit
// file value at all, so it is refused rather than mangled.
func systemdQuote(arg string) (string, error) {
	if strings.ContainsAny(arg, "\n\r") {
		return "", fmt.Errorf("serviceinstall: a systemd unit cannot carry a newline in %q", arg)
	}
	escaped := strings.ReplaceAll(arg, "%", "%%")
	if !strings.ContainsAny(escaped, " \t\"\\'") {
		return escaped, nil
	}
	escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`, nil
}
