package serviceinstall

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ServiceName is the unit's name on Linux, without the .service suffix.
const ServiceName = "agent-overflow"

// LaunchdLabel is the LaunchAgent's label on macOS. It is deliberately NOT the
// app bundle's identifier: a Mac can legitimately run the desktop app and a
// serve-mode agent at once, and two things launchd must tell apart cannot
// share a label.
const LaunchdLabel = "com.agentoverflow.serve"

// Runner runs one external command and reports what it said.
//
// A non-zero exit is DATA, not an error: `systemctl is-active` answers
// "inactive" with exit 3, and a status command that treated that as a failure
// would report nothing where the manager gave a clear answer. err is reserved
// for "the command could not be run at all" — no such binary, no permission,
// a cancelled context.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (output string, exitCode int, err error)
}

// ExecRunner is the one implementation that touches the machine. Nothing in
// any test may construct it.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, int, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitCode(), nil
	}
	if err != nil {
		return text, -1, fmt.Errorf("%s: %w", name, err)
	}
	return text, 0, nil
}

// Config describes the service to manage. Every host fact is a field rather
// than a call into the process, so a test describes a machine instead of
// running on one.
type Config struct {
	// GOOS is the host's runtime.GOOS.
	GOOS string

	// Executable is the absolute path to the agent-overflow binary the unit
	// starts. It is written into the unit verbatim: whatever path is named
	// here is the one the service runs after every reboot, so naming a stable
	// path is how an operator makes "replace the file" an update.
	Executable string

	// HomeDir is the user's home directory, absolute.
	HomeDir string

	// ConfigHome is $XDG_CONFIG_HOME. Empty means <HomeDir>/.config. Linux
	// only; systemd reads user units from $XDG_CONFIG_HOME/systemd/user, so a
	// host that sets it and a unit written to ~/.config would not meet.
	ConfigHome string

	// UID is the numeric user id launchd needs to name a GUI domain. Required
	// on darwin, ignored elsewhere.
	UID string

	// Listen, when set, is passed to serve as --listen. Usually it should NOT
	// be: a flag in the unit overrides the saved network settings on every
	// boot, so Settings -> Network stops being able to move the backend.
	Listen string
}

// Status is what a service manager reports about the unit.
type Status struct {
	Manager   string // how to describe the manager to a person
	UnitPath  string
	Installed bool   // the unit file is on disk
	Enabled   bool   // the manager will start it without being asked
	Running   bool   // it is up now
	Detail    string // the manager's own words, for everything else
}

// Manager is one host's service manager.
type Manager interface {
	// Name describes the manager to a person.
	Name() string
	// UnitPath is where the unit file lives.
	UnitPath() string
	// UnitContents is the file that would be written, generated and
	// inspectable without touching the disk.
	UnitContents() (string, error)
	// Install writes the unit and tells the manager to run it from now on.
	Install(ctx context.Context) error
	// Uninstall stops it, forgets it, and removes the unit.
	Uninstall(ctx context.Context) error
	// Stop halts the service without forgetting it. `service update` needs
	// it: staging a version and moving the durable selection are the two
	// things that must not happen while a supervisor is reading them.
	Stop(ctx context.Context) error
	// Start runs it again after a Stop.
	Start(ctx context.Context) error
	// Status reports what the manager currently thinks.
	Status(ctx context.Context) (Status, error)
	// Notes are things a person should know after a successful install.
	Notes() []string
}

// ErrUnsupported is returned for a host that has no per-user service manager
// this package drives. It carries the reason, which is the useful part.
var ErrUnsupported = errors.New("serviceinstall: unsupported host")

// New returns the manager for a host, or an error explaining why that host has
// none. The runner is required.
func New(config Config, runner Runner) (Manager, error) {
	if runner == nil {
		return nil, errors.New("serviceinstall: a Runner is required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	switch config.GOOS {
	case "linux":
		return &systemdManager{config: config, runner: runner}, nil
	case "darwin":
		if strings.TrimSpace(config.UID) == "" {
			return nil, errors.New("serviceinstall: a numeric UID is required on darwin (launchd names a GUI domain by it)")
		}
		return &launchdManager{config: config, runner: runner}, nil
	case "windows":
		return nil, fmt.Errorf("%w: on Windows, Agent Overflow is a launcher that already supervises "+
			"its backend inside WSL. Install the service inside the WSL distribution instead, using "+
			"the Linux binary there", ErrUnsupported)
	default:
		return nil, fmt.Errorf("%w: no per-user service manager is wired up for %s. Run "+
			"`agent-overflow serve` under whatever supervisor this system uses", ErrUnsupported, config.GOOS)
	}
}

func (c Config) validate() error {
	if strings.TrimSpace(c.GOOS) == "" {
		return errors.New("serviceinstall: GOOS is required")
	}
	// Checked before the GOOS switch so an unsupported host is reported as
	// unsupported rather than as a bad path.
	if c.GOOS != "linux" && c.GOOS != "darwin" {
		return nil
	}
	if !filepath.IsAbs(c.Executable) {
		return fmt.Errorf("serviceinstall: the executable path must be absolute, got %q. A service "+
			"manager starts with no working directory of yours", c.Executable)
	}
	if !filepath.IsAbs(c.HomeDir) {
		return fmt.Errorf("serviceinstall: the home directory must be absolute, got %q", c.HomeDir)
	}
	if c.ConfigHome != "" && !filepath.IsAbs(c.ConfigHome) {
		return fmt.Errorf("serviceinstall: the config home must be absolute, got %q", c.ConfigHome)
	}
	if strings.ContainsAny(c.Listen, " \t\n") {
		return fmt.Errorf("serviceinstall: the listen address must not contain whitespace, got %q", c.Listen)
	}
	return nil
}

// SuperviseVerb is the boot mode the unit starts.
//
// The unit runs the SUPERVISOR, not the backend directly, and that is the
// whole shape of remote update: the process a service manager selects has to
// be the stable one, because it is what decides which version runs and what
// happens when a new one does not work. It spawns `serve` itself
// (`internal/supervise`). A unit still naming `serve` would be an install
// whose backend cannot be updated over the wire at all — which is why install
// always rewrites the unit rather than leaving an existing one alone.
//
// Spelled here rather than imported from package main, which no package may
// import; `main_entry.go`'s superviseVerb is pinned to this constant by a
// drift test.
const SuperviseVerb = "supervise"

// serveArgs is the argv the unit starts, executable first.
func (c Config) serveArgs() []string {
	args := []string{c.Executable, SuperviseVerb}
	if c.Listen != "" {
		args = append(args, "--listen", c.Listen)
	}
	return args
}

// configHome resolves ConfigHome's default.
func (c Config) configHome() string {
	if c.ConfigHome != "" {
		return c.ConfigHome
	}
	return filepath.Join(c.HomeDir, ".config")
}
