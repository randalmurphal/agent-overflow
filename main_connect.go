//go:build !nogui

// main_connect.go resolves what `agent-overflow --connect <target>`
// names, and runs the pairing ceremony in the terminal when the answer is
// "a machine this installation has never met".
//
// It sits beside runClient (main_desktop.go) under the same build tag
// rather than in main.go, because the headless WSL payload has no
// `--connect` mode at all and linking the device client into it would
// carry a pairing ceremony nothing there can run.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/deviceclient"
)

// deviceProfileDirName holds this installation's device identity: the one
// key every backend knows it by, and one session file per backend it has
// paired with.
//
// Under the app config root and NOT under --data-dir, which `--connect`
// refuses to be combined with anyway (main_entry.go): a data dir is one
// backend's database, while the device key is this installation's name on
// every backend it has ever met.
const deviceProfileDirName = "device"

func deviceProfileDir() (string, error) {
	root := bootSettingsDir()
	if root == "" {
		return "", errors.New("no config directory is resolvable, so this device has nowhere to keep its pairing")
	}
	return filepath.Join(root, deviceProfileDirName), nil
}

// prepareConnection turns the `--connect` argument into the stub
// configuration that attaches to it.
//
// Three forms reach this, and each is recognised by its STRUCTURE rather
// than by trying the next one after a failure:
//
//  1. `ws://` or `wss://` — the same-host attach that has always worked:
//     an endpoint plus the backend's launch token, composed by whoever
//     is running both processes. Untouched by this function.
//  2. a pairing link, which either carries a `#` fragment (the URL the
//     settings pane shows, or the `#pair=…` a person copies out of it) or
//     is the bare payload a typed code gives.
//  3. anything else, which names a backend this installation is already
//     paired with: its id, its endpoint, or just `host:port`.
//
// The order between 2 and 3 is the one place structure alone is not
// enough, because a pairing URL's authority IS a stored profile's
// authority — pairing again with a machine you are already paired with is
// an ordinary thing to do. So a fragment settles it first, a stored
// profile answers next, and a bare payload that decodes into a real link
// is tried last. Nothing here falls back from a link that decoded and
// then failed: that is a refusal to report, not a spelling to re-guess.
func prepareConnection(ctx context.Context, target string) (clientmode.Config, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return clientmode.Config{}, errors.New("name a pairing link, a paired backend, or a ws:// endpoint")
	}
	if parsed, err := url.Parse(trimmed); err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss") {
		return clientmode.ParseConnectURL(trimmed)
	}

	dir, err := deviceProfileDir()
	if err != nil {
		return clientmode.Config{}, err
	}
	return resolveConnection(ctx, dir, trimmed)
}

// resolveConnection is the half of prepareConnection that needs a profile
// directory, taking it as an argument so a test drives the ordering above
// against a temporary one instead of the machine's real device identity.
func resolveConnection(ctx context.Context, dir, trimmed string) (clientmode.Config, error) {
	if strings.Contains(trimmed, "#") {
		return pairAndAttach(ctx, dir, trimmed)
	}

	session, err := deviceclient.Resolve(dir, trimmed)
	switch {
	case err == nil:
		return attachPaired(ctx, dir, session)
	case !errors.Is(err, deviceclient.ErrNoSession):
		// An ambiguous name, or a profile directory that could not be
		// read. Both are answers, not misses.
		return clientmode.Config{}, err
	}
	if _, decodeErr := deviceclient.DecodeLink(trimmed); decodeErr == nil {
		return pairAndAttach(ctx, dir, trimmed)
	}
	return clientmode.Config{}, fmt.Errorf(
		"%q is not a pairing link, a paired backend, or a ws:// endpoint", trimmed)
}

// pairAndAttach spends a pairing link and blocks until the owner confirms.
//
// Everything printed here is printed once and in order, because the
// person is reading it while looking at another screen: which machine
// this is, the number to compare, what to do with it, and then the wait.
func pairAndAttach(ctx context.Context, dir, raw string) (clientmode.Config, error) {
	link, err := deviceclient.DecodeLink(raw)
	if err != nil {
		return clientmode.Config{}, err
	}
	name := backendDisplayName(link.BackendName, link.Endpoint)
	fmt.Printf("Pairing with %s.\n", backendDisplay(link.BackendName, link.Endpoint))

	client, pairing, err := deviceclient.Pair(ctx, dir, link, deviceLabel(), runtime.GOOS)
	if err != nil {
		return clientmode.Config{}, err
	}
	// The number is the backend's answer, derived from the key this
	// process just proved it holds, so it can only be printed after the
	// redemption. Printed exactly as the backend spelled it: the other
	// screen shows the same six characters, and a client that regrouped
	// them would be asking the person to compare two different strings.
	fmt.Printf("Verification number: %s\n", pairing.VerificationNumber)
	fmt.Println("Compare it with the number on the computer running Agent Overflow, and confirm it there.")
	fmt.Println("Waiting for confirmation.")

	if err := client.AwaitActivation(ctx); err != nil {
		return clientmode.Config{}, err
	}
	fmt.Printf("Paired. Attaching to %s.\n", name)
	return pairedConfig(client)
}

// attachPaired opens a stored session and proves it still works before a
// window exists.
//
// The proof is one socket ticket, which is the cheapest authenticated
// call this client has and the only one that renews a stale credential on
// the way. Spending it here rather than discovering the answer on the
// first carried upgrade is the difference between a sentence in the
// terminal and a window that reconnects forever: the SPA's ladder cannot
// tell "this device was removed" from "the network is down", and the
// person who ran this command is standing right here.
func attachPaired(ctx context.Context, dir string, session deviceclient.Session) (clientmode.Config, error) {
	client, err := deviceclient.Open(dir, session)
	if err != nil {
		return clientmode.Config{}, err
	}
	if _, err := client.Ticket(ctx); err != nil {
		return clientmode.Config{}, err
	}
	fmt.Printf("Attaching to %s.\n", backendDisplay(session.BackendName, session.Endpoint))
	return pairedConfig(client)
}

// pairedConfig is the stub configuration for a paired backend: the
// upgrade endpoint with no ticket on it, and the client that mints one
// per carried handshake.
func pairedConfig(client *deviceclient.Client) (clientmode.Config, error) {
	wsURL, err := client.WebSocketURL()
	if err != nil {
		return clientmode.Config{}, err
	}
	return clientmode.Config{WSURL: wsURL, Paired: client}, nil
}

// backendDisplay names a backend the way a person would: what it calls
// itself and where it is, or just where it is when it published no name.
func backendDisplay(name, endpoint string) string {
	if name == "" {
		return endpoint
	}
	return name + " at " + endpoint
}

// backendDisplayName is the short form, for the line after the address
// has already been printed.
func backendDisplayName(name, endpoint string) string {
	if name == "" {
		return endpoint
	}
	return name
}

// deviceLabel is what this installation asks to be called in the owner's
// device list. The hostname is what the person confirming the pairing
// recognises; a machine that will not tell us its name gets a generic
// label rather than an empty row.
func deviceLabel() string {
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return strings.TrimSpace(host)
	}
	return "Agent Overflow desktop"
}

// fatalConnect ends a `--connect` run with the reason and, where there is
// one, the next thing to do about it.
//
// Two failures get that extra line and they share a remedy: a session the
// backend will not honour again, and a certificate that is not the one
// this device pinned. Both mean this device has to be enrolled afresh,
// and neither is something retrying can fix.
func fatalConnect(err error) {
	if errors.Is(err, deviceclient.ErrSessionEnded) || errors.Is(err, deviceclient.ErrCertificateMismatch) {
		fatalf("--connect: %v\nOn the computer running Agent Overflow, open Settings, then Network, and create a new pairing link.", err)
	}
	fatalf("--connect: %v", err)
}
