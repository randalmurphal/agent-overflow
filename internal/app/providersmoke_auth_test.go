//go:build providersmoke

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/provider"
)

func TestProviderSmokeAuthAcceptsClaudeWithoutIdentity(t *testing.T) {
	binary := writeProviderSmokeAuthCLI(t, `{"loggedIn":true}`, 0)
	smoke := providerSmokeClaudeCase()
	smoke.probeAccount = func(*App) (provider.AccountInfo, error) {
		t.Error("Claude preflight started a session probe")
		return provider.AccountInfo{}, nil
	}
	preflightProviderAuth(t, nil, smoke, binary)
}

func TestProviderSmokeClaudeAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		name, output      string
		loggedIn, wantErr bool
		exitCode          int
	}{
		{"logged in", `{"loggedIn":true}`, true, false, 0},
		{"logged out", `{"loggedIn":false}`, false, false, 1},
		{"missing state", `{}`, false, true, 0},
		{"invalid state", `not JSON`, false, true, 0},
		{"command failure", `failed`, false, true, 2},
		{"unexpected exit", `{"loggedIn":false}`, false, true, 2},
		{"failed with login", `{"loggedIn":true}`, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := writeProviderSmokeAuthCLI(t, tc.output, tc.exitCode)
			loggedIn, err := providerSmokeClaudeAuthStatus(t.Context(), binary)
			if loggedIn != tc.loggedIn || (err != nil) != tc.wantErr {
				t.Fatalf("auth status = %v, %v; want %v, error=%v", loggedIn, err, tc.loggedIn, tc.wantErr)
			}
		})
	}
}

func writeProviderSmokeAuthCLI(t *testing.T, output string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path+".json", []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = 'auth status' ] || exit 2\ncat \"$0.json\"\nexit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
