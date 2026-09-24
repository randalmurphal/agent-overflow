package wsllauncher

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// updatePayloadTimeout bounds one payload file operation inside the distro.
const updatePayloadTimeout = 2 * time.Minute

// UpdatePayloads is the WSL half of UpdateHost: update commands and payload
// files inside a distro, through wsl.exe. Runner's settings apply to every
// command; its Distro is the one each call names.
type UpdatePayloads struct {
	Runner UpdateCommandRunner
}

// RunCommand runs one update command of the backend at payload.
func (p UpdatePayloads) RunCommand(ctx context.Context, distro, payload, command string, args []string, onProgress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	runner := p.Runner
	runner.Distro = distro
	return runner.Run(ctx, payload, command, args, onProgress)
}

// CommitPayload renames the staged payload over the stable path. Both are on
// the distro's filesystem, so the rename is atomic, and a staged payload that
// is already gone was moved by an earlier commit.
func (p UpdatePayloads) CommitPayload(ctx context.Context, record supervise.LauncherRecord) error {
	return p.shell(ctx, record.Distro, `if [ -e "$1" ]; then mv -f -- "$1" "$2"; fi`, record.StagedPayload, record.StablePayload)
}

// RemoveStagedPayload deletes the staged payload if it is present.
func (p UpdatePayloads) RemoveStagedPayload(ctx context.Context, record supervise.LauncherRecord) error {
	return p.shell(ctx, record.Distro, `rm -f -- "$1"`, record.StagedPayload)
}

// Preflight asks the backend at payload what it is.
func (p UpdatePayloads) Preflight(ctx context.Context, distro, payload string) (supervise.Preflight, error) {
	ctx, cancel := context.WithTimeout(ctx, updatePayloadTimeout)
	defer cancel()
	out, err := p.output(ctx, distro, payload, supervise.PreflightSubcommand)
	if err != nil {
		return supervise.Preflight{}, err
	}
	return supervise.ParsePreflight(out)
}

// shell runs script with /bin/sh inside the distro. The paths are
// positional arguments, never spliced into the script.
func (p UpdatePayloads) shell(ctx context.Context, distro, script string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, updatePayloadTimeout)
	defer cancel()
	_, err := p.output(ctx, distro, append([]string{"/bin/sh", "-c", "set -e; " + script, "agent-overflow-update"}, args...)...)
	return err
}

func (p UpdatePayloads) output(ctx context.Context, distro string, argv ...string) (string, error) {
	if distro == "" {
		return "", fmt.Errorf("wsllauncher: no distro to run %s in", argv[0])
	}
	runner := p.Runner.Command
	if runner == nil {
		runner = exec.CommandContext
	}
	cmd := runner(ctx, "wsl.exe", append([]string{"-d", distro, "--exec"}, argv...)...)
	hideConsole(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wsl.exe %s: %w (stderr: %s)", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
