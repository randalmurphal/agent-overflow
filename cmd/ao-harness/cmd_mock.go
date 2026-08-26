package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harnessclient"
)

// advanceReportTimeout is how long `mock advance` waits for the mock to
// report what its command did. The control channel is a loopback long
// poll and the mock answers in milliseconds; three seconds is generous
// enough that a busy mock still reports and short enough that a caller
// who typed a gate name at an exited mock finds out immediately.
const advanceReportTimeout = 3 * time.Second

var mockSubcommands = []string{"list", "advance", "emit", "exit"}

func runMock(e *env, args []string) error {
	if done, err := groupHelp(e, "mock", args, mockSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("mock needs a subcommand: %s", strings.Join(mockSubcommands, ", "))
	}
	switch args[0] {
	case "list":
		return mockList(e, args[1:])
	case "advance":
		return mockAdvance(e, args[1:])
	case "emit":
		return mockEmit(e, args[1:])
	case "exit":
		return mockExit(e, args[1:])
	default:
		return usagef("unknown mock subcommand %q (want %s)", args[0], strings.Join(mockSubcommands, ", "))
	}
}

// mockRow is the printed subset of control.MockInfo. Declared locally
// rather than decoded into the backend's struct because the gate fields
// are additions a given backend may not carry yet, and a CLI that
// refused to render an older instance's mock list would be useless
// exactly when a version mismatch is what you are debugging.
type mockRow struct {
	MockID       string `json:"mockId"`
	Registration struct {
		Protocol string `json:"protocol"`
		PID      int    `json:"pid"`
		Cwd      string `json:"cwd"`
	} `json:"registration"`
	Scenario string `json:"scenario"`
	Exited   bool   `json:"exited"`
	// OpenGate names the waitSignal gate the mock is currently parked on,
	// and PendingAdvances the releases it has buffered for gates that are
	// not open yet. Absent from an older backend, which renders as "-".
	OpenGate        string   `json:"openGate"`
	PendingAdvances []string `json:"pendingAdvances"`
}

func listMocks(ctx context.Context, client *harnessclient.Client) ([]mockRow, json.RawMessage, error) {
	raw, err := client.Call(ctx, "HarnessListMocks")
	if err != nil {
		return nil, nil, err
	}
	var mocks []mockRow
	if err := json.Unmarshal(raw, &mocks); err != nil {
		return nil, nil, fmt.Errorf("decode mocks: %w", err)
	}
	return mocks, raw, nil
}

func mockList(e *env, args []string) error {
	flags := e.newFlagSet("mock list")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("mock list takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		mocks, raw, err := listMocks(ctx, client)
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		if len(mocks) == 0 {
			e.printf("no mocks registered (a mock registers when a session spawns one)\n")
			return nil
		}
		rows := make([][]string, 0, len(mocks))
		for _, mock := range mocks {
			state := "live"
			if mock.Exited {
				state = "exited"
			}
			rows = append(rows, []string{
				mock.MockID, mock.Registration.Protocol, state, mock.Scenario,
				gateCell(mock), fmt.Sprint(mock.Registration.PID), truncate(mock.Registration.Cwd, 40),
			})
		}
		return e.table([]string{"ID", "PROTOCOL", "STATE", "SCENARIO", "GATE", "PID", "CWD"}, rows)
	})
}

// gateCell renders where a mock is parked. `mock advance` is the one
// command whose correct argument is invisible without it: the gate name
// lives in the scenario document, and "which one is open right now" is
// the only question worth asking before releasing one.
func gateCell(mock mockRow) string {
	switch {
	case mock.OpenGate != "" && len(mock.PendingAdvances) > 0:
		return fmt.Sprintf("%s (+%d buffered)", mock.OpenGate, len(mock.PendingAdvances))
	case mock.OpenGate != "":
		return mock.OpenGate
	case len(mock.PendingAdvances) > 0:
		return fmt.Sprintf("- (+%d buffered)", len(mock.PendingAdvances))
	default:
		return "-"
	}
}

// mockAdvance releases a waitSignal gate. Two ergonomics that were the
// difference between "usable" and "read the source first": the gate name
// is positional, matching `mock emit`'s own grammar (`mock emit <id>
// <line>`), and the mock id is optional when there is exactly one live
// mock — which is every single-session debugging run.
func mockAdvance(e *env, args []string) error {
	flags := e.newFlagSet("mock advance [mock-id] [gate]")
	name := flags.String("name", "", "release this named waitSignal gate (default: whichever gate is open)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 2 {
		return usagef("mock advance takes at most a mock id and a gate name (got %v)", rest)
	}

	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		mocks, _, err := listMocks(ctx, client)
		if err != nil {
			return err
		}
		mockID, gate, err := advanceArgs(mocks, rest, *name)
		if err != nil {
			return err
		}
		return e.issueAdvance(ctx, client, mockID, gate)
	})
}

// advanceArgs resolves the positional grammar against what is actually
// registered, so `mock advance first-delta` (one live mock, a gate name)
// and `mock advance mock-2 first-delta` both mean what they look like.
func advanceArgs(mocks []mockRow, rest []string, flagName string) (mockID, gate string, err error) {
	var live []mockRow
	byID := map[string]bool{}
	for _, mock := range mocks {
		byID[mock.MockID] = true
		if !mock.Exited {
			live = append(live, mock)
		}
	}
	gate = flagName

	switch len(rest) {
	case 2:
		mockID = rest[0]
		if gate != "" && gate != rest[1] {
			return "", "", usagef("mock advance got two different gate names (--name %q and %q)", gate, rest[1])
		}
		gate = rest[1]
	case 1:
		// One word: a mock id if a registered mock carries it, otherwise a
		// gate name for the only live mock. Checking the registry rather
		// than guessing by shape is what makes a gate called "mock-1" work.
		if byID[rest[0]] {
			mockID = rest[0]
		} else {
			if gate != "" && gate != rest[0] {
				return "", "", usagef("mock advance got two different gate names (--name %q and %q)", gate, rest[0])
			}
			gate = rest[0]
		}
	}

	if mockID != "" {
		return mockID, gate, nil
	}
	switch len(live) {
	case 1:
		return live[0].MockID, gate, nil
	case 0:
		return "", "", fmt.Errorf("no live mock to advance (see `ao-harness mock list`)")
	default:
		names := make([]string, 0, len(live))
		for _, mock := range live {
			names = append(names, mock.MockID)
		}
		return "", "", usagef("%d mocks are live; name one: %s", len(live), strings.Join(names, ", "))
	}
}

// issueAdvance sends the command and then reports what it DID, by
// waiting on the mock's own progress channel.
//
// The old fire-and-forget line ("advance -> mock-1") was true of the RPC
// and said nothing about the gate: a name that matches no open gate is
// BUFFERED by the mock, silently, and the caller's next move is usually
// to conclude the harness is wedged. The wait is parked before the
// command is issued, because a mock releases a gate faster than the RPC
// returns.
func (e *env) issueAdvance(ctx context.Context, client *harnessclient.Client, mockID, gate string) error {
	channel := string(eventchan.HarnessMock)
	// Subscribe failures are not fatal: the command is still worth
	// issuing, and the outcome line degrades to the old one.
	subscribed := client.Subscribe(ctx, channel) == nil
	var awaiting *harnessclient.Awaiting
	if subscribed {
		awaiting = client.Await(channel, func(ev harnessclient.Event) bool {
			if ev.Gap {
				return false
			}
			report := decodeMockReport(ev.Data)
			return report.MockID == mockID && (report.Kind == "advance_released" || report.Kind == "advance_buffered" || report.Kind == "fixture_error")
		})
	}

	cmd := control.Command{Type: control.CommandAdvance, Name: gate}
	if _, err := client.Call(ctx, "HarnessMockCommand", mockID, cmd); err != nil {
		if awaiting != nil {
			awaiting.Close()
		}
		return err
	}
	if awaiting == nil {
		return e.printAdvanceOutcome(mockID, cmd, mockReport{})
	}

	waitCtx, cancel := context.WithTimeout(ctx, advanceReportTimeout)
	defer cancel()
	event, err := awaiting.Wait(waitCtx)
	if err != nil {
		return e.printAdvanceOutcome(mockID, cmd, mockReport{})
	}
	return e.printAdvanceOutcome(mockID, cmd, decodeMockReport(event.Data))
}

// mockReport is the harness:mock frame, decoded only as far as the three
// advance outcomes need. Every other field stays on the wire.
type mockReport struct {
	MockID string `json:"mockId"`
	Kind   string `json:"kind"`
	Gate   string `json:"gate"`
	// OpenGate is carried by advance_buffered: the gate that IS open,
	// which is the one fact a caller who named the wrong one needs.
	OpenGate string `json:"openGate"`
	Detail   string `json:"detail"`
}

func decodeMockReport(data json.RawMessage) mockReport {
	var report mockReport
	_ = json.Unmarshal(data, &report)
	return report
}

func (e *env) printAdvanceOutcome(mockID string, cmd control.Command, report mockReport) error {
	if e.jsonOutput() {
		return e.writeJSON(map[string]any{"mockId": mockID, "command": cmd, "outcome": report.Kind, "gate": report.Gate, "openGate": report.OpenGate})
	}
	switch report.Kind {
	case "advance_released":
		e.printf("released gate %q on %s\n", orDash(report.Gate), mockID)
	case "advance_buffered":
		e.printf("buffered %q (open gate is %q)\n", report.Gate, report.OpenGate)
	case "fixture_error":
		e.printf("advance -> %s failed: %s\n", mockID, report.Detail)
	default:
		e.printf("%s -> %s\n", cmd.Type, mockID)
		e.printf("note: no report arrived within %s — check `ao-harness mock list` for the open gate\n", advanceReportTimeout)
	}
	return nil
}

func mockEmit(e *env, args []string) error {
	flags := e.newFlagSet("mock emit <mock-id> <wire-line...>")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return usagef("mock emit needs a mock id and at least one wire line")
	}
	lines := rest[1:]
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			return usagef("wire line %d is empty", i+1)
		}
	}
	return sendMockCommand(e, rest[0], control.Command{Type: control.CommandEmit, Lines: lines})
}

func mockExit(e *env, args []string) error {
	flags := e.newFlagSet("mock exit <mock-id>")
	code := flags.Int("code", 0, "exit code for the mock process")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	mockID, err := oneMockID("mock exit", rest)
	if err != nil {
		return err
	}
	return sendMockCommand(e, mockID, control.Command{Type: control.CommandExit, Code: *code})
}

func oneMockID(command string, rest []string) (string, error) {
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return "", usagef("%s needs exactly one mock id (see `ao-harness mock list`)", command)
	}
	return rest[0], nil
}

func sendMockCommand(e *env, mockID string, cmd control.Command) error {
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if _, err := client.Call(ctx, "HarnessMockCommand", mockID, cmd); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"mockId": mockID, "command": cmd})
		}
		e.printf("%s -> %s\n", cmd.Type, mockID)
		return nil
	})
}
