package main

import (
	"fmt"
	"log"
	"os"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// Fallback env vars for running the mock without a harness control
// channel (bare Go tests, manual poking).
const (
	envScenarioFile = "AO_MOCK_SCENARIO_FILE"
	envFixtureRoot  = "AO_MOCK_FIXTURE_ROOT"
)

// scenarioSource is the resolved script plus (when control-driven) the
// live client used for commands and progress reports.
type scenarioSource struct {
	sc          *scenario.Scenario
	fixtureRoot string
	client      *control.Client // nil outside a harness
}

// acquireScenario resolves the scenario in priority order: control
// channel, then AO_MOCK_SCENARIO_FILE, then the built-in fallback when
// neither names one. A scenario that was named but cannot run (unreadable,
// invalid, or for the other provider) is an error: running a different
// script than the one assigned would pass or fail a spec for the wrong
// reason. A control plane that refuses the registration still degrades to
// the file or the fallback, as before: the app must never hang on a mock
// whose harness is gone.
func acquireScenario(protocol, cwd, resumeRef string) (scenarioSource, error) {
	if client, ok := control.FromEnv(); ok {
		resp, err := client.Register(control.Registration{
			Protocol:  protocol,
			Cwd:       cwd,
			PID:       os.Getpid(),
			ResumeRef: resumeRef,
		})
		if err != nil {
			log.Printf("control register failed: %v (falling back to %s/builtin scenario)", err, envScenarioFile)
		} else {
			sc, err := parseAssignedScenario(resp.Scenario, protocol, "control-assigned")
			if err != nil {
				return scenarioSource{}, err
			}
			return scenarioSource{sc: sc, fixtureRoot: resp.FixtureRoot, client: client}, nil
		}
	}

	if path := os.Getenv(envScenarioFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return scenarioSource{}, fmt.Errorf("read %s=%q: %w", envScenarioFile, path, err)
		}
		sc, err := parseAssignedScenario(data, protocol, path)
		if err != nil {
			return scenarioSource{}, err
		}
		return scenarioSource{sc: sc, fixtureRoot: os.Getenv(envFixtureRoot)}, nil
	}

	log.Printf("no control channel and no %s; running builtin %s fallback scenario", envScenarioFile, protocol)
	return scenarioSource{sc: builtinScenario(protocol)}, nil
}

// parseAssignedScenario parses and provider-checks a scenario document.
func parseAssignedScenario(data []byte, protocol, origin string) (*scenario.Scenario, error) {
	sc, err := scenario.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s scenario invalid: %w", origin, err)
	}
	if sc.Provider != protocol {
		return nil, fmt.Errorf("%s scenario %q targets provider %q but this mock runs %q", origin, sc.Name, sc.Provider, protocol)
	}
	return sc, nil
}

// fallbackResponseText is the assistant text both builtin scenarios
// stream, so a harness run that lost its scenario is obvious in the UI.
const fallbackResponseText = "ao-mockprovider fallback response"

func builtinScenario(protocol string) *scenario.Scenario {
	if protocol == scenario.ProviderCodex {
		return builtinCodexScenario()
	}
	return builtinClaudeScenario()
}

// builtinClaudeScenario carries no system/init line — the claude
// adapter emits init + the user-envelope echo per turn (see
// claudeAdapter's doc). The message_start frame is load-bearing: the
// app's parser records its message id so the coalesced assistant
// envelope is deduped instead of recovered as a second text row.
func builtinClaudeScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "builtin-claude-fallback",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Label: "fallback", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
			`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-mock-${TURN}","role":"assistant"}}}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + fallbackResponseText + `"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
			`{"type":"assistant","message":{"id":"msg-mock-${TURN}","role":"assistant","content":[{"type":"text","text":"` + fallbackResponseText + `"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		}}}}}},
		AfterTurns: "repeatLast",
	}
}

func builtinCodexScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "builtin-codex-fallback",
		Provider: scenario.ProviderCodex,
		Turns: []scenario.Turn{{Label: "fallback", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
			`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
			`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"msg-${TURN}","type":"agentMessage","text":""}}}`,
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"id":"msg-${TURN}","type":"agentMessage","text":"` + fallbackResponseText + `"}}}`,
			`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
		}}}}}},
		AfterTurns: "repeatLast",
	}
}
