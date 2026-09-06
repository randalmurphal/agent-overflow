package aocli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/network"
)

type pairingCaller interface {
	Call(context.Context, string, any, ...any) error
}
type consoleInvite struct {
	LinkID      string `json:"linkId"`
	URL         string `json:"url"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}
type consolePairingStatus struct {
	State              string `json:"state"`
	VerificationNumber string `json:"verificationNumber"`
	DeviceLabel        string `json:"deviceLabel"`
}

func pairCommand(args []string, root string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-overflow pair", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	lan := flags.Bool("lan", false, "enable LAN access")
	machine := flags.Bool("json", false, "emit setup records")
	configRoot := flags.String("config-root", root, "override the app data root")
	class := flags.String("class", "browser", "device class")
	wait := flags.Duration("wait", 0, "wait for backend startup (e.g. 30s)")
	if code, done := parseServiceFlags(flags, args, pairUsage, stdout, stderr); done {
		return code
	}
	if *class != "browser" && *class != "android" && *class != "desktop" {
		return operationalError(stderr, errors.New("class must be browser, android or desktop"))
	}
	if *wait < 0 || *wait > 2*time.Minute {
		return operationalError(stderr, errors.New("wait must be between 0s and 2m"))
	}
	if testing.Testing() && *configRoot == "" {
		return operationalError(stderr, errors.New("tests must supply an isolated config root for local control"))
	}
	resolvedRoot, err := resolveConfigRoot(*configRoot)
	if err != nil {
		return operationalError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client, err := connectLocalControl(ctx, resolvedRoot, *wait)
	if err != nil {
		return operationalError(stderr, err)
	}
	defer client.Close()
	if err := runPairingConsole(ctx, client, *class, *lan, *machine, stdin, stdout, 500*time.Millisecond); err != nil {
		return operationalError(stderr, err)
	}
	return exitOK
}

// Retry discovery only. Once connected, a failed pairing mutation is never
// retried implicitly, so one invocation cannot create multiple invitations.
func connectLocalControl(ctx context.Context, root string, wait time.Duration) (*localcontrol.Client, error) {
	deadline := time.Now().Add(wait)
	for {
		endpoint, err := localcontrol.Read(root)
		if err == nil {
			dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			client, dialErr := localcontrol.Dial(dialCtx, endpoint)
			cancel()
			if dialErr == nil {
				return client, nil
			}
			err = dialErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		timer := time.NewTimer(min(remaining, 200*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func runPairingConsole(ctx context.Context, client pairingCaller, class string, lan, machine bool, stdin io.Reader, stdout io.Writer, poll time.Duration) error {
	call := func(method string, result any, params ...any) error {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return client.Call(callCtx, method, result, params...)
	}
	if lan {
		var settings network.Settings
		if err := call("GetNetworkSettings", &settings); err != nil {
			return err
		}
		if !settings.BindAll {
			settings.BindAll = true
			if err := call("SetNetworkSettings", nil, settings); err != nil {
				return err
			}
		}
	}
	var invite consoleInvite
	if err := call("MintDevicePairing", &invite, class, "full"); err != nil {
		return err
	}
	confirmed := false
	defer func() {
		if confirmed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = client.Call(cleanup, "CancelDevicePairing", nil, invite.LinkID)
	}()
	output := func(kind, message string, value any) error {
		if machine {
			return json.NewEncoder(stdout).Encode(struct {
				Type string `json:"type"`
				Data any    `json:"data,omitempty"`
			}{kind, value})
		}
		_, err := fmt.Fprintln(stdout, message)
		return err
	}
	if err := output("invitation", "Open this link on the device you want to connect:\n"+invite.URL, invite); err != nil {
		return err
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		var status consolePairingStatus
		if err := call("DevicePairingStatus", &status, invite.LinkID); err != nil {
			return err
		}
		switch status.State {
		case "confirmed":
			confirmed = true
			return output("paired", "Device connected. The backend keeps running when this console closes.", nil)
		case "redeemed":
			if len(status.VerificationNumber) != 6 {
				return errors.New("the backend returned an invalid verification number")
			}
			if err := output("verification", fmt.Sprintf("%s is connecting. Compare %s on both devices, then enter those six digits here:", status.DeviceLabel, status.VerificationNumber), status); err != nil {
				return err
			}
			answer := make(chan string, 1)
			go func() {
				line, _ := bufio.NewReader(io.LimitReader(stdin, 256)).ReadString('\n')
				answer <- strings.TrimSpace(line)
			}()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case entered := <-answer:
				if entered != status.VerificationNumber {
					return errors.New("verification did not match; the pairing was canceled")
				}
			}
			if err := call("ConfirmDevicePairing", nil, invite.LinkID); err != nil {
				return err
			}
			confirmed = true
			return output("paired", "Device connected. The backend keeps running when this console closes.", nil)
		case "pending":
		default:
			return fmt.Errorf("the pairing is %s; run agent-overflow pair to try again", status.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
