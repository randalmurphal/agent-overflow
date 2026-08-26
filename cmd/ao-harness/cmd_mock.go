package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harnessclient"
)

func runMock(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("mock needs a subcommand: list, advance, emit, exit")
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
		return usagef("unknown mock subcommand %q (want list, advance, emit, exit)", args[0])
	}
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
		raw, err := client.Call(ctx, "HarnessListMocks")
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		var mocks []control.MockInfo
		if err := json.Unmarshal(raw, &mocks); err != nil {
			return fmt.Errorf("decode mocks: %w", err)
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
				fmt.Sprint(mock.Registration.PID), truncate(mock.Registration.Cwd, 48),
			})
		}
		return e.table([]string{"ID", "PROTOCOL", "STATE", "SCENARIO", "PID", "CWD"}, rows)
	})
}

func mockAdvance(e *env, args []string) error {
	flags := e.newFlagSet("mock advance")
	name := flags.String("name", "", "release this named waitSignal gate (default: whichever gate is open)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	mockID, err := oneMockID("mock advance", rest)
	if err != nil {
		return err
	}
	return sendMockCommand(e, mockID, control.Command{Type: control.CommandAdvance, Name: *name})
}

func mockEmit(e *env, args []string) error {
	flags := e.newFlagSet("mock emit")
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
	flags := e.newFlagSet("mock exit")
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
