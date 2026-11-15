package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerupdate"
)

// GetProviderUpdates returns each provider CLI's update state as last
// checked. It reads memory only: no subprocess and no network request.
//
//ao:scope access:admin
//ao:route selected
func (a *App) GetProviderUpdates() ([]providerupdate.Status, error) {
	return a.providerUpdateService().Snapshot(), nil
}

// CheckProviderUpdates asks each provider's release host again and returns
// the result, which it also publishes on provider:updates.
//
//ao:scope access:admin
//ao:route selected
func (a *App) CheckProviderUpdates() ([]providerupdate.Status, error) {
	return a.providerUpdateService().Check(), nil
}

// GetProviderUpdateNotes returns the provider's release notes between the
// installed version and the latest one, newest first.
//
//ao:scope access:admin
//ao:route selected
func (a *App) GetProviderUpdateNotes(ctx context.Context, providerName string) (providerupdate.Notes, error) {
	return a.providerUpdateService().Notes(ctx, providerName)
}

// RunProviderUpdate starts the provider CLI's own update command and returns
// once the run is claimed. The client follows provider:updates for the
// outcome.
//
// `//ao:stepup`: this replaces the provider CLI this machine runs, the same
// class of act as RequestServiceUpdate.
//
//ao:scope access:admin
//ao:stepup
//ao:route selected
func (a *App) RunProviderUpdate(ctx context.Context, providerName string) error {
	if err := a.requireStepUp(ctx, "updating a provider CLI on this computer"); err != nil {
		return err
	}
	// Held only while the run is claimed. A running update is named by
	// updateWorkReason, so a restart to update waits for it, and one cannot
	// start after that wait found the computer idle.
	endAdmission, err := a.workAdmission.begin(ctx)
	if err != nil {
		return err
	}
	defer endAdmission()
	return a.providerUpdateService().Run(providerName)
}

// providerUpdateService builds the service on first use, so every App,
// including a test's composite literal, has one.
func (a *App) providerUpdateService() *providerupdate.Service {
	a.providerUpdatesOnce.Do(func() {
		a.providerUpdates = providerupdate.New(providerupdate.Deps{
			Binary:    a.providerUpdateBinary,
			Installed: a.providerUpdateInstalledVersion,
			Env:       a.providerCustomEnv,
			WorkDir:   os.UserHomeDir,
			Runner:    a.providerUpdateCommand,
			// The sweep records the new binary, refreshes its model catalog
			// and flags sessions still running the old build, now rather
			// than at the next watch tick.
			AfterRun: a.sweepProviderBinaries,
			Client:   a.providerUpdateClient,
			Emit: func(statuses []providerupdate.Status) {
				a.emit(eventchan.ProviderUpdates, statuses)
			},
			Now: time.Now,
		})
	})
	return a.providerUpdates
}

// providerUpdateBinary is the provider's install as sessions run it: the
// binary they spawn, the npm launcher it was resolved from, and the file
// behind it.
func (a *App) providerUpdateBinary(providerName string) providerupdate.Binary {
	var binary providerupdate.Binary
	switch providerName {
	case string(provider.Claude):
		binary.Path = a.providerBinaryPath(providerName)
	case string(provider.Codex):
		native := a.codexBinary()
		binary.Path, binary.Launcher = native.Path, native.Wrapper
	default:
		return binary
	}
	if identity, ok := a.resolveProviderBinaryIdentity(providerName); ok {
		binary.ResolvedPath = identity.path
	}
	return binary
}

// providerUpdateInstalledVersion reads the installed version through the
// binary watch, which probes only a binary that changed and, on a version
// change, refreshes the catalog before recording it.
func (a *App) providerUpdateInstalledVersion(providerName string) string {
	a.refreshInstalledProviderVersion(providerName)
	return a.installedProviderVersion(providerName)
}

// providerUpdateCommand runs one provider update or doctor command. Inside
// a test binary it is refused unless a fake is assigned: the command
// replaces the provider CLI on the machine running the tests.
func (a *App) providerUpdateCommand(ctx context.Context, command providerupdate.Command) (providerupdate.Output, error) {
	if a.providerUpdateRunner != nil {
		return a.providerUpdateRunner(ctx, command)
	}
	if testing.Testing() {
		return providerupdate.Output{}, errors.New(
			"tests must not run a real provider CLI for provider updates; assign app.providerUpdateRunner a fake",
		)
	}
	return runProviderUpdateGroup(ctx, command)
}

// providerUpdateOutputTail bounds what a command's output keeps: its end,
// which is where an updater says why it failed.
const providerUpdateOutputTail = 64 << 10

// runProviderUpdateGroup runs command as its own process group, so ending
// ctx stops the package manager an updater starts (npm, brew) and not just
// the updater, and nothing it started outlives the run.
func runProviderUpdateGroup(ctx context.Context, command providerupdate.Command) (providerupdate.Output, error) {
	tail := procutil.NewTailBuffer(providerUpdateOutputTail)
	outcome, err := procutil.RunGroup(ctx, procutil.GroupCommand{
		Dir:    command.Dir,
		Env:    provider.BuildEnvironment(command.Env),
		Argv:   append([]string{command.Binary}, command.Args...),
		Output: tail,
	})
	text := tail.String()
	// Neither changes what the user does next, so they are noted in the
	// output the user reads rather than failing the command.
	if outcome.Leftovers {
		text += "\n[processes the command left running were stopped]"
	}
	if outputErr := outcome.OutputErr(); outputErr != nil {
		text += fmt.Sprintf("\n[output may be incomplete: %v]", outputErr)
	}
	out := providerupdate.Output{Text: text, ExitCode: outcome.ExitCode}
	switch {
	case errors.Is(err, procutil.ErrGroupSurvived):
		return out, errors.Join(providerupdate.ErrCommandSurvived, ctx.Err())
	case outcome.Stopped:
		return out, ctx.Err()
	case err == nil:
		return out, nil
	}
	if _, exited := errors.AsType[*exec.ExitError](err); exited {
		return out, nil
	}
	return out, err
}

// errProviderUpdateNetwork refuses a release host request from a test
// binary whose App was given no isolated client.
var errProviderUpdateNetwork = errors.New(
	"tests must not reach the provider release hosts; assign app.providerUpdateClientOverride an isolated client",
)

var providerUpdateRefusingClient = &http.Client{Transport: roundTripRefusal{err: errProviderUpdateNetwork}}

type roundTripRefusal struct{ err error }

func (r roundTripRefusal) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		if err := req.Body.Close(); err != nil {
			return nil, errors.Join(r.err, err)
		}
	}
	return nil, r.err
}

// providerUpdateHTTPClient is production's client for the release hosts.
// Each request also carries its lookup's deadline.
var providerUpdateHTTPClient = &http.Client{Timeout: time.Minute}

// providerUpdateClient is the release hosts' client: the isolated one when
// set, a refusing one in a test binary, else providerUpdateHTTPClient.
func (a *App) providerUpdateClient() *http.Client {
	if a.providerUpdateClientOverride != nil {
		return a.providerUpdateClientOverride
	}
	if testing.Testing() {
		return providerUpdateRefusingClient
	}
	return providerUpdateHTTPClient
}
