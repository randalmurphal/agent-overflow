//go:build darwin

// ao-darwin-harness creates a uniquely identified app bundle for one
// windowed harness run, then replaces itself with the copied executable.
// The replacement process verifies that its bundle id matches the root and
// nonce before Wails is initialized.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/harness/darwinbundle"
)

const runNonceEnv = "AO_HARNESS_BUNDLE_NONCE"
const expectedBundleIDEnv = "AO_EXPECTED_BUNDLE_ID"
const disclaimResponsibilityEnv = "AO_HARNESS_DISCLAIM_RESPONSIBILITY"

func main() {
	flags := flag.NewFlagSet("ao-darwin-harness", flag.ExitOnError)
	binary := flags.String("binary", "", "path to the raw Agent Overflow executable")
	dataRoot := flags.String("data-root", "", "isolated harness data root")
	plist := flags.String("plist", "build/darwin/Info.dev.plist", "Info.plist template")
	driver := flags.String("driver", "", "path to the ao-harness driver")
	mockProvider := flags.String("mock-provider", "", "path to ao-mockprovider (default: beside --binary)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		fatal(err)
	}
	backendArgs := flags.Args()
	if len(backendArgs) == 0 {
		fatal(fmt.Errorf("arguments after -- are required"))
	}
	if strings.TrimSpace(*driver) == "" {
		fatal(fmt.Errorf("--driver is required"))
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		fatal(fmt.Errorf("generate run nonce: %w", err))
	}
	nonce := hex.EncodeToString(nonceBytes)
	appPath, err := darwinbundle.Create(*binary, *dataRoot, *plist, nonce)
	if err != nil {
		fatal(err)
	}
	executable := appPath + "/Contents/MacOS/agent-overflow"
	resolvedMock := strings.TrimSpace(*mockProvider)
	if resolvedMock == "" {
		resolvedMock = filepath.Join(filepath.Dir(*binary), "ao-mockprovider")
	}
	upArgs, err := supervisedUpArgs(backendArgs, executable, *dataRoot, resolvedMock)
	if err != nil {
		fatal(err)
	}
	env := append([]string{}, os.Environ()...)
	env = append(env, runNonceEnv+"="+nonce, expectedBundleIDEnv+"="+darwinbundle.BundleID(*dataRoot, nonce), disclaimResponsibilityEnv+"=1")
	runErr := runSupervised(*driver, upArgs, env, *dataRoot)
	cleanupErr := darwinbundle.Cleanup(*dataRoot, appPath)
	if err := errors.Join(runErr, cleanupErr); err != nil {
		fatal(err)
	}
}

func supervisedUpArgs(backendArgs []string, executable, dataRoot, mockProvider string) ([]string, error) {
	soak := slices.Contains(backendArgs, "--soak")
	autopilot := slices.Contains(backendArgs, "--autopilot")
	for _, arg := range backendArgs {
		switch arg {
		case "--harness", "--soak", "--autopilot", "--window":
		default:
			return nil, fmt.Errorf("unsupported window backend argument %q", arg)
		}
	}
	if !soak && !slices.Contains(backendArgs, "--harness") {
		return nil, errors.New("window backend arguments must select --harness or --soak")
	}
	if autopilot && !soak {
		return nil, errors.New("--autopilot requires --soak")
	}
	args := []string{"up", "--window", "--binary", executable, "--data-dir", dataRoot}
	if strings.TrimSpace(mockProvider) != "" {
		args = append(args, "--mock-provider", mockProvider)
	}
	if soak {
		args = append(args, "--soak")
	}
	if autopilot {
		args = append(args, "--autopilot")
	}
	return args, nil
}

func runSupervised(driver string, upArgs []string, env []string, dataRoot string) error {
	ctx, stop := signalContext()
	defer stop()
	cmd := exec.CommandContext(ctx, driver, upArgs...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stopSupervised(driver, dataRoot)
		}
		return fmt.Errorf("start supervised harness: %w", err)
	}
	for {
		if ctx.Err() != nil {
			if err := stopSupervised(driver, dataRoot); err != nil {
				return err
			}
			return nil
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		probe := exec.CommandContext(probeCtx, driver, "info", "--instance", dataRoot)
		probe.Stdout = nil
		probe.Stderr = nil
		err := probe.Run()
		cancel()
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func stopSupervised(driver, dataRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, driver, "down", "--instance", dataRoot)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop supervised harness: %w", err)
	}
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "darwin harness:", err)
	os.Exit(2)
}
