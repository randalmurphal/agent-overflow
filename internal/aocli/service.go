package aocli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/serviceinstall"
	"agent-overflow/internal/supervise"
)

// serviceEnv is everything the service command needs to know about the machine
// it is running on. It is a PARAMETER rather than a set of calls inside the
// command, so the tests describe a host instead of running on one — and so
// there is no package-level seam a test has to remember to reset.
//
// The two lookups that can fail carry their error rather than being resolved
// eagerly: `service --help` must work on a host where os.Executable() does not.
type serviceEnv struct {
	goos          string
	executable    string
	executableErr error
	home          string
	homeErr       error
	configHome    string
	uid           string
	runner        serviceinstall.Runner
	// configRoot is the app-managed directory the backend keeps its data in,
	// and therefore where its launch state and staged versions live. Carried
	// with its error for the same reason executable is: `service --help` must
	// work on a host where it cannot be resolved.
	configRoot    string
	configRootErr error
}

// hostServiceEnv reads the real machine. lookupEnv supplies XDG_CONFIG_HOME,
// which is where systemd actually reads user units from.
func hostServiceEnv(lookupEnv func(string) (string, bool)) serviceEnv {
	env := serviceEnv{
		goos:   runtime.GOOS,
		uid:    strconv.Itoa(os.Getuid()),
		runner: serviceinstall.ExecRunner{},
	}
	env.executable, env.executableErr = os.Executable()
	env.home, env.homeErr = os.UserHomeDir()
	env.configRoot, env.configRootErr = appdirs.Root()
	if configHome, ok := lookupEnv("XDG_CONFIG_HOME"); ok {
		env.configHome = configHome
	}
	return env
}

func serviceCommand(args []string, env serviceEnv, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_ = writeOutput(stderr, serviceUsage)
		return exitError
	}
	switch args[0] {
	case "help", "--help", "-h":
		if err := writeOutput(stdout, serviceUsage); err != nil {
			return operationalError(stderr, err)
		}
		return exitOK
	case "install":
		return serviceInstall(args[1:], env, stdout, stderr)
	case "start", "stop":
		return serviceControl(args[0], args[1:], env, stdout, stderr)
	case "update":
		return serviceUpdate(args[1:], env, stdout, stderr)
	case "uninstall":
		return serviceUninstall(args[1:], env, stdout, stderr)
	case "status":
		return serviceStatus(args[1:], env, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agent-overflow service: unknown command %q\n", args[0])
	_ = writeOutput(stderr, serviceUsage)
	return exitError
}

// Control an already installed service without rewriting its unit or data.
func serviceControl(action string, args []string, env serviceEnv, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow service "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	usage := "Usage: agent-overflow service " + action + "\n"
	if code, done := parseServiceFlags(flags, args, usage, stdout, stderr); done {
		return code
	}
	if action == "start" && env.configRootErr == nil && env.configRoot != "" {
		// A desktop app may already own this data root. Reuse its backend;
		// starting a service alongside it would create a second provider owner.
		if endpoint, err := localcontrol.Read(env.configRoot); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			client, err := localcontrol.Dial(ctx, endpoint)
			cancel()
			if err == nil {
				client.Close()
				fmt.Fprintln(stdout, "Agent Overflow is already running. Use agent-overflow pair to connect.")
				return exitOK
			}
		}
	}
	manager, _, code := serviceManager(env, "", "", stderr)
	if manager == nil {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := manager.Status(ctx)
	if err != nil {
		return operationalError(stderr, err)
	}
	if !status.Installed {
		return operationalError(stderr, fmt.Errorf("no service is installed; run agent-overflow service install first"))
	}
	if action == "start" {
		err = manager.Start(ctx)
	} else {
		err = manager.Stop(ctx)
	}
	if err != nil {
		return operationalError(stderr, err)
	}
	if action == "start" {
		fmt.Fprintln(stdout, "Started the Agent Overflow service.")
	} else {
		fmt.Fprintln(stdout, "Stopped the Agent Overflow service. It remains installed for the next login.")
	}
	return exitOK
}

func serviceInstall(args []string, env serviceEnv, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow service install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "", "bind address to pass to serve")
	binary := flags.String("binary", "", "path to the agent-overflow binary the service starts")
	if code, done := parseServiceFlags(flags, args, serviceInstallUsage, stdout, stderr); done {
		return code
	}

	manager, config, code := serviceManager(env, *binary, *listen, stderr)
	if manager == nil {
		return code
	}
	if err := manager.Install(context.Background()); err != nil {
		return operationalError(stderr, err)
	}

	fmt.Fprintf(stdout, "Installed the Agent Overflow service.\n")
	fmt.Fprintf(stdout, "  Manager: %s\n", manager.Name())
	fmt.Fprintf(stdout, "  Unit:    %s\n", manager.UnitPath())
	fmt.Fprintf(stdout, "  Starts:  %s\n", strings.Join(serviceStartCommand(config), " "))
	for _, note := range manager.Notes() {
		fmt.Fprintf(stdout, "\n%s\n", note)
	}
	return exitOK
}

// serviceUpdate is the LOCAL update path: the operator is standing at the
// machine, so there is no trial, no snapshot and no automatic rollback.
//
// The sequence is t3code's local command and the order is the point. Stop the
// unit FIRST: staging a version and moving the durable selection are exactly
// the two things a running supervisor is reading, and a selection written
// underneath one would be read on its next restart with no way to tell when.
// Then stage, then select, then start — so a failure at any step leaves the
// service stopped on a state that still names a version that works.
//
// This is also how a supervisor is replaced, which is the other half of the
// protocol boundary: a target that needs a newer update protocol than the
// installed supervisor is refused over the wire, and this is the command that
// refusal names.
func serviceUpdate(args []string, env serviceEnv, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow service update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	binary := flags.String("binary", "", "the binary to install as the new version")
	if code, done := parseServiceFlags(flags, args, serviceUpdateUsage, stdout, stderr); done {
		return code
	}

	manager, config, code := serviceManager(env, *binary, "", stderr)
	if manager == nil {
		return code
	}
	if env.configRootErr != nil {
		fmt.Fprintf(stderr, "agent-overflow service: cannot find the config root: %v\n", env.configRootErr)
		return exitError
	}
	layout, err := supervise.NewLayout(env.configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}

	// Ask the binary what it is, in its own process. It is the only thing that
	// can answer, and an answer that comes back also proves the file runs on
	// this host at all — which is worth learning before the service is stopped
	// rather than after.
	answer, err := servicePreflight(config.Executable, env.runner)
	if err != nil {
		return operationalError(stderr, err)
	}
	if err := supervise.ValidVersion(answer.Version); err != nil {
		fmt.Fprintf(stderr, "agent-overflow service: %s reports version %q, which cannot name a directory: %v\n",
			config.Executable, answer.Version, err)
		return exitError
	}

	if err := manager.Stop(context.Background()); err != nil {
		return operationalError(stderr, err)
	}
	if err := supervise.StageBinary(layout, answer.Version, config.Executable); err != nil {
		return operationalError(stderr, err)
	}
	state, err := supervise.Adopt(answer.Version)
	if err != nil {
		return operationalError(stderr, err)
	}
	if err := supervise.SaveState(layout, state); err != nil {
		return operationalError(stderr, err)
	}
	if err := manager.Start(context.Background()); err != nil {
		return operationalError(stderr, err)
	}

	fmt.Fprintf(stdout, "Updated the Agent Overflow service to version %s.\n", answer.Version)
	fmt.Fprintf(stdout, "  Manager: %s\n", manager.Name())
	fmt.Fprintf(stdout, "  Staged:  %s\n", filepath.Join(mustVersionDir(layout, answer.Version), supervise.BinaryName))
	fmt.Fprintf(stdout, "  Starts:  %s\n", strings.Join(serviceStartCommand(config), " "))
	return exitOK
}

// servicePreflight runs the staged binary's own answer through the injected
// Runner, so a `service update` test describes a binary rather than executing
// one — the same posture every other command this host verb runs already has.
func servicePreflight(binary string, runner serviceinstall.Runner) (supervise.Preflight, error) {
	output, code, err := runner.Run(context.Background(), binary, supervise.PreflightSubcommand)
	if err != nil {
		return supervise.Preflight{}, fmt.Errorf("running %s %s: %w", binary, supervise.PreflightSubcommand, err)
	}
	if code != 0 {
		return supervise.Preflight{}, fmt.Errorf(
			"%s %s exited %d, so it is not an Agent Overflow binary this can install%s",
			binary, supervise.PreflightSubcommand, code, indentedDetail(output))
	}
	return supervise.ParsePreflight(output)
}

func indentedDetail(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	return ":\n  " + strings.ReplaceAll(strings.TrimSpace(output), "\n", "\n  ")
}

// mustVersionDir is for printing only: the version was validated above, so the
// error branch is unreachable and an empty path is a better outcome than a
// second failure path in the success message.
func mustVersionDir(layout supervise.Layout, version string) string {
	dir, err := layout.VersionDir(version)
	if err != nil {
		return ""
	}
	return dir
}

func serviceUninstall(args []string, env serviceEnv, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow service uninstall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if code, done := parseServiceFlags(flags, args, serviceUninstallUsage, stdout, stderr); done {
		return code
	}

	manager, _, code := serviceManager(env, "", "", stderr)
	if manager == nil {
		return code
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		return operationalError(stderr, err)
	}
	fmt.Fprintf(stdout, "Removed the Agent Overflow service (%s).\n", manager.UnitPath())
	fmt.Fprintf(stdout, "Nothing else was touched: the config root, its data and its credentials are still there.\n")
	return exitOK
}

// serviceStatus exits 1 when the service is not running. That is this CLI's
// documented "the asked-about thing said no", and it is what makes `service
// status` usable in a shell conditional.
func serviceStatus(args []string, env serviceEnv, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow service status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if code, done := parseServiceFlags(flags, args, serviceStatusUsage, stdout, stderr); done {
		return code
	}

	manager, _, code := serviceManager(env, "", "", stderr)
	if manager == nil {
		return code
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		return operationalError(stderr, err)
	}

	fmt.Fprintf(stdout, "Manager:   %s\n", status.Manager)
	fmt.Fprintf(stdout, "Unit:      %s (%s)\n", status.UnitPath, presentAbsent(status.Installed))
	fmt.Fprintf(stdout, "Starts up: %s\n", yesNo(status.Enabled))
	if status.Detail != "" {
		fmt.Fprintf(stdout, "Running:   %s (%s)\n", yesNo(status.Running), status.Detail)
	} else {
		fmt.Fprintf(stdout, "Running:   %s\n", yesNo(status.Running))
	}
	if !status.Running {
		return exitFindings
	}
	return exitOK
}

// serviceManager builds the manager for this host, or reports why it cannot
// and returns the exit code to use. A nil manager means "return code".
func serviceManager(env serviceEnv, binary, listen string, stderr io.Writer) (serviceinstall.Manager, serviceinstall.Config, int) {
	config := serviceinstall.Config{
		GOOS:       env.goos,
		Executable: binary,
		ConfigHome: env.configHome,
		UID:        env.uid,
		Listen:     listen,
	}
	if config.Executable == "" {
		if env.executableErr != nil {
			fmt.Fprintf(stderr, "agent-overflow service: cannot find this binary's own path (%v). "+
				"Name it with --binary.\n", env.executableErr)
			return nil, config, exitError
		}
		config.Executable = env.executable
	}
	if env.homeErr != nil {
		fmt.Fprintf(stderr, "agent-overflow service: cannot find your home directory: %v\n", env.homeErr)
		return nil, config, exitError
	}
	config.HomeDir = env.home

	manager, err := serviceinstall.New(config, env.runner)
	if err != nil {
		// An unsupported host is not a usage mistake and not a failure of
		// this machine: it is an answer. It still exits non-zero, because a
		// script that asked for a service did not get one.
		fmt.Fprintf(stderr, "agent-overflow service: %v\n", err)
		if errors.Is(err, serviceinstall.ErrUnsupported) {
			return nil, config, exitFindings
		}
		return nil, config, exitError
	}
	return manager, config, exitOK
}

// serviceStartCommand is what the unit runs, for printing back to a person.
// The verb comes from serviceinstall rather than a literal, so this cannot
// print one thing while the unit file says another.
func serviceStartCommand(config serviceinstall.Config) []string {
	args := []string{config.Executable, serviceinstall.SuperviseVerb}
	if config.Listen != "" {
		args = append(args, "--listen", config.Listen)
	}
	return args
}

// parseServiceFlags folds the shared parse/help/positional handling. done
// reports whether the caller should return code immediately.
func parseServiceFlags(flags *flag.FlagSet, args []string, usage string, stdout, stderr io.Writer) (int, bool) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if writeErr := writeOutput(stdout, usage); writeErr != nil {
				return operationalError(stderr, writeErr), true
			}
			return exitOK, true
		}
		fmt.Fprintf(stderr, "%s: %v\n", flags.Name(), err)
		_ = writeOutput(stderr, usage)
		return exitError, true
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected positional arguments\n", flags.Name())
		_ = writeOutput(stderr, usage)
		return exitError, true
	}
	return exitOK, false
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func presentAbsent(value bool) string {
	if value {
		return "installed"
	}
	return "not installed"
}
