//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStartupFailurePageUsesObservedCauseWithoutLeakingErrorContent(t *testing.T) {
	const secret = `private-token-123<script>alert("server body")</script>`
	for _, tc := range []struct {
		name, want string
		err        error
		forwarding bool
	}{
		{"spawn", "process could not start inside WSL", errLaunchFailed, false},
		{"unreachable", "No HTTP response arrived", errBackendUnreachable, true},
		{"invalid manifest", "startup response was not valid", errInvalidBootstrap, false},
		{"not ready", "HTTP 503", errBackendNotReady, false},
		{"HTTP rejection", "HTTP 404", bootstrapHTTPError{StatusCode: 404, URL: secret}, false},
		{"HTTP startup failure", "HTTP 500", bootstrapHTTPError{StatusCode: 500, URL: secret}, false},
		{"invalid status", "local backend could not be opened", bootstrapHTTPError{StatusCode: -1234, URL: secret}, false},
		{"other failure", "local backend could not be opened", errors.New(secret), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := string(startupFailureHTML(fmt.Errorf("%s: %w", secret, tc.err)))
			if !strings.Contains(body, tc.want) {
				t.Fatalf("page does not explain %q: %s", tc.want, body)
			}
			if strings.Contains(body, "localhostForwarding") != tc.forwarding {
				t.Fatalf("forwarding guidance does not match observed failure: %s", body)
			}
			for _, forbidden := range []string{secret, "private-token", "<script>", "fresh port", "already retried", "backend is running", "Windows reached", "-1234"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("page leaked or claimed %q", forbidden)
				}
			}
		})
	}
}
