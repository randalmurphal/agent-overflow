//go:build !nogui

package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/deviceclient"
)

// What `--connect` was handed decides which of three shapes it is, and the
// decision is made once from structure rather than by attempting each in
// turn. These cases pin the decisions that need no network: an endpoint is
// answered without touching the profile at all, and a name that resolves
// to nothing, or to two backends, is reported rather than re-guessed.
//
// The paired branches themselves are proved end to end against a real
// backend in internal/app's app_deviceclient_integration_test.go.

func TestPrepareConnectionAnswersAnEndpointWithoutTouchingTheProfile(t *testing.T) {
	for name, raw := range map[string]string{
		"cleartext": "ws://192.168.1.5:8317/ws?token=launch-credential",
		"tls":       "wss://192.168.1.5:8317/ws?token=launch-credential",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := prepareConnection(t.Context(), raw)
			if err != nil {
				t.Fatalf("prepareConnection(%q): %v", raw, err)
			}
			if cfg.Token != "launch-credential" {
				t.Fatalf("Token = %q, want the one the URL carried", cfg.Token)
			}
			if cfg.Paired != nil {
				t.Error("an endpoint attach resolved a paired device session")
			}
			if strings.Contains(cfg.WSURL, "token=") {
				t.Errorf("WSURL = %q, want the credential split out of it", cfg.WSURL)
			}
		})
	}
}

func TestPrepareConnectionRefusesAnEmptyTarget(t *testing.T) {
	if _, err := prepareConnection(t.Context(), "   "); err == nil {
		t.Fatal("an empty --connect argument was accepted")
	}
}

// A name that is none of the three forms is a refusal that says so, rather
// than a pairing attempt against something that is not a link.
func TestResolveConnectionNamesTheThreeFormsWhenNothingMatches(t *testing.T) {
	_, err := resolveConnection(t.Context(), t.TempDir(), "studio")
	if err == nil {
		t.Fatal("an unpaired name was accepted")
	}
	if !strings.Contains(err.Error(), "pairing link") {
		t.Fatalf("the refusal does not say what may be named: %v", err)
	}
}

// An ambiguous name is an ANSWER, not a miss: falling through to the
// pairing branch would attach to whichever backend sorted first.
func TestResolveConnectionReportsAnAmbiguousName(t *testing.T) {
	dir := t.TempDir()
	for _, backendID := range []string{"backend-0001", "backend-0002"} {
		if err := deviceclient.SaveSession(dir, deviceclient.Session{
			BackendID:  backendID,
			Endpoint:   "http://192.168.1.5:8317",
			SessionID:  "sess-" + backendID,
			Credential: "ao1." + backendID,
		}); err != nil {
			t.Fatalf("SaveSession(%s): %v", backendID, err)
		}
	}

	_, err := resolveConnection(t.Context(), dir, "192.168.1.5:8317")
	if err == nil {
		t.Fatal("a name matching two paired backends was accepted")
	}
	for _, want := range []string{"backend-0001", "backend-0002"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

// A fragment settles the pairing form before a stored profile is consulted,
// and a fragment that does not decode is reported as itself. Nothing falls
// back from a link that failed — that is a refusal, not a spelling to
// re-guess.
func TestResolveConnectionReportsAnUnreadablePairingLink(t *testing.T) {
	_, err := resolveConnection(t.Context(), t.TempDir(), "http://192.168.1.5:8317/#pair=not-a-payload")
	if err == nil {
		t.Fatal("an unreadable pairing link was accepted")
	}
	if strings.Contains(err.Error(), "is not a pairing link") {
		t.Fatalf("a failed link decode fell through to the unknown-name refusal: %v", err)
	}
}

func TestBackendDisplayNamesWhatThePersonWouldRecognise(t *testing.T) {
	if got := backendDisplay("Studio", "http://192.168.1.5:8317"); got != "Studio at http://192.168.1.5:8317" {
		t.Errorf("backendDisplay = %q", got)
	}
	if got := backendDisplay("", "http://192.168.1.5:8317"); got != "http://192.168.1.5:8317" {
		t.Errorf("backendDisplay with no name = %q", got)
	}
	if got := backendDisplayName("Studio", "http://192.168.1.5:8317"); got != "Studio" {
		t.Errorf("backendDisplayName = %q", got)
	}
	if got := backendDisplayName("", "http://192.168.1.5:8317"); got != "http://192.168.1.5:8317" {
		t.Errorf("backendDisplayName with no name = %q", got)
	}
}

// The label is what the owner reads in their device list before confirming,
// so an installation that cannot name itself must still offer something.
func TestDeviceLabelIsNeverEmpty(t *testing.T) {
	if strings.TrimSpace(deviceLabel()) == "" {
		t.Fatal("this installation would ask to be enrolled under an empty label")
	}
}
