package serviceinstall

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type launchdManager struct {
	config Config
	runner Runner
}

func (m *launchdManager) Name() string { return "launchd (user agent)" }

func (m *launchdManager) UnitPath() string {
	return filepath.Join(m.config.HomeDir, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// target is how launchctl names this agent: a service inside the user's GUI
// domain. gui/<uid> rather than user/<uid> because a LaunchAgent belongs to
// the login session, which is also why it does not run before anyone logs in.
func (m *launchdManager) target() string { return "gui/" + m.config.UID + "/" + LaunchdLabel }

func (m *launchdManager) domain() string { return "gui/" + m.config.UID }

func (m *launchdManager) logPath() string {
	return filepath.Join(m.config.HomeDir, "Library", "Logs", "agent-overflow-serve.log")
}

// UnitContents generates the LaunchAgent plist.
//
// KeepAlive/SuccessfulExit=false is launchd's Restart=on-failure: relaunch
// when it dies badly, leave it alone when it exits cleanly, which is what
// makes stopping the service possible. RunAtLoad starts it at login.
//
// The XML is assembled rather than marshalled because plist's schema is
// positional dict pairs, which no Go struct maps to cleanly — but every value
// goes through plistString, so a path with an ampersand or an angle bracket in
// it produces a valid file rather than a broken one.
func (m *launchdManager) UnitContents() (string, error) {
	var args bytes.Buffer
	for _, arg := range m.config.serveArgs() {
		text, err := plistString(arg)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&args, "\t\t<string>%s</string>\n", text)
	}
	logPath, err := plistString(m.logPath())
	if err != nil {
		return "", err
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + strings.TrimRight(args.String(), "\n") + `
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + logPath + `</string>
	<key>StandardErrorPath</key>
	<string>` + logPath + `</string>
</dict>
</plist>
`, nil
}

func (m *launchdManager) Install(ctx context.Context) error {
	contents, err := m.UnitContents()
	if err != nil {
		return err
	}
	path := m.UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("serviceinstall: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(m.logPath()), 0o755); err != nil {
		return fmt.Errorf("serviceinstall: create %s: %w", filepath.Dir(m.logPath()), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("serviceinstall: write %s: %w", path, err)
	}
	// Not mustRun: on a first install there is nothing loaded to boot out,
	// and launchctl says so with a non-zero exit. On a REINSTALL this is the
	// step that makes the new plist take effect, because bootstrap refuses a
	// label that is already loaded.
	if _, _, err := m.runner.Run(ctx, "launchctl", "bootout", m.target()); err != nil {
		return err
	}
	if err := m.mustRun(ctx, "launchctl", "bootstrap", m.domain(), path); err != nil {
		return err
	}
	// enable is separate from bootstrap and outlives it: a service the user
	// once disabled stays disabled through a reinstall otherwise.
	return m.mustRun(ctx, "launchctl", "enable", m.target())
}

func (m *launchdManager) Uninstall(ctx context.Context) error {
	if _, _, err := m.runner.Run(ctx, "launchctl", "bootout", m.target()); err != nil {
		return err
	}
	if err := os.Remove(m.UnitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("serviceinstall: remove %s: %w", m.UnitPath(), err)
	}
	return nil
}

// Stop halts the agent without unloading its plist.
//
// `launchctl kill` rather than `bootout`: bootout unloads the label entirely,
// and a Start after one would have to bootstrap the plist again — which is
// install's job, not update's. KeepAlive only relaunches on a BAD exit, so a
// SIGTERM the backend handles cleanly leaves it stopped, which is what this
// asks for. Not mustRun: an agent that is not running is the state wanted.
func (m *launchdManager) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, code, err := m.runner.Run(ctx, "launchctl", "kill", "SIGTERM", m.target())
	if err != nil {
		return err
	}
	// kill acknowledges a signal, not process exit. Never stage an update
	// while the old supervisor can still read its selection file.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, statusCode, err := m.runner.Run(ctx, "launchctl", "print", m.target())
		if err != nil {
			return err
		}
		if statusCode != 0 {
			if code != 0 {
				return fmt.Errorf("serviceinstall: launchctl stop: %s", output)
			}
			return nil
		}
		active := launchdState(state) == "running"
		for line := range strings.SplitSeq(state, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "pid = ") {
				active = true
			}
		}
		if !active {
			return nil
		}
		if code != 0 {
			return fmt.Errorf("serviceinstall: launchctl stop: %s", output)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("serviceinstall: waiting for the backend to stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *launchdManager) Start(ctx context.Context) error {
	return m.mustRun(ctx, "launchctl", "kickstart", m.target())
}

func (m *launchdManager) Status(ctx context.Context) (Status, error) {
	status := Status{Manager: m.Name(), UnitPath: m.UnitPath()}
	if _, err := os.Stat(status.UnitPath); err == nil {
		status.Installed = true
	} else if !os.IsNotExist(err) {
		return status, fmt.Errorf("serviceinstall: stat %s: %w", status.UnitPath, err)
	}

	// `launchctl print` exits non-zero for a label launchd does not hold,
	// which is the answer "not loaded" rather than a failure.
	output, code, err := m.runner.Run(ctx, "launchctl", "print", m.target())
	if err != nil {
		return status, err
	}
	if code != 0 {
		status.Detail = "not loaded"
		return status, nil
	}
	status.Enabled = true
	status.Detail = launchdState(output)
	status.Running = status.Detail == "running"
	return status, nil
}

func (m *launchdManager) Notes() []string {
	return []string{
		"A LaunchAgent runs in your login session: it starts when you log in and stops " +
			"when you log out. A Mac that reboots to the login window is not serving until " +
			"somebody signs in.",
		"Logs: tail -f " + m.logPath(),
	}
}

func (m *launchdManager) mustRun(ctx context.Context, name string, args ...string) error {
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

// launchdState reads the `state = running` line out of `launchctl print`.
// Anything else is reported verbatim: launchd's vocabulary here is larger than
// this package should pretend to know, and a word it has not seen is more
// useful to a person than "unknown".
func launchdState(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "state" {
			continue
		}
		if state := strings.TrimSpace(value); state != "" {
			return state
		}
	}
	return "loaded"
}

// plistString escapes one text value for the generated XML.
func plistString(value string) (string, error) {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return "", fmt.Errorf("serviceinstall: escape %q: %w", value, err)
	}
	return buf.String(), nil
}
