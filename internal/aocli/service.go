package aocli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"agent-overflow/internal/serviceinstall"
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
	case "uninstall":
		return serviceUninstall(args[1:], env, stdout, stderr)
	case "status":
		return serviceStatus(args[1:], env, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agent-overflow service: unknown command %q\n", args[0])
	_ = writeOutput(stderr, serviceUsage)
	return exitError
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
func serviceStartCommand(config serviceinstall.Config) []string {
	args := []string{config.Executable, "serve"}
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
