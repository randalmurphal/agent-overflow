package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"

	"agent-overflow/internal/providerschema"
)

// One-shot text generation is the third invocation shape the app spawns a
// provider binary in, beside a streaming session and the account probe. It is
// NOT a session: no scenario, no control-channel registration, no turn
// lifecycle — the process reads a prompt on stdin, writes one structured
// answer, and exits.
//
// The two argv shapes come from internal/textgen (RunClaude / RunCodex):
//
//	claude -p --output-format json --json-schema <inline JSON> \
//	       --safe-mode --no-session-persistence [--model M] [--effort E] [...]
//	codex exec --ephemeral --ignore-user-config --skip-git-repo-check \
//	       -s read-only --model M --output-schema <path> \
//	       --output-last-message <path> [...] -
//
// Without this mode a harness thread's generated title (and every commit
// message) came from an UNMOCKED spawn: the Claude branch fell through to the
// NDJSON session adapter, which answered a plain-text prompt with nothing, and
// the Codex branch was sniffed as Claude entirely because `codex exec` carries
// no `app-server` argument.

// isClaudeTextGenInvocation matches textgen.RunClaude's argv. `-p` plus
// `--output-format json` is unambiguous: a streaming session always asks for
// `--output-format stream-json` and never passes `-p`, and the account probe
// is discriminated before this by `--max-turns 0`.
func isClaudeTextGenInvocation(args []string) bool {
	return containsArg(args, "-p") && flagValue(args, "--output-format") == "json"
}

// isCodexTextGenInvocation matches textgen.RunCodex's argv. It has to run
// BEFORE the protocol sniff, which reads anything without `app-server` as
// Claude — `codex exec` has no such marker. `exec` plus `--ephemeral` is the
// pair no other Codex spawn in this app carries.
func isCodexTextGenInvocation(args []string) bool {
	return containsArg(args, "exec") && containsArg(args, "--ephemeral")
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// runClaudeTextGen answers `claude -p --output-format json --json-schema …`.
//
// The real CLI prints one JSON object per line and carries the structured
// answer on the LAST one, inside a `structured_output` key
// (textgen.DecodeClaudeStructuredLastLine is the only reader). The envelope is
// the ordinary `result` shape so the line is recognisable in a captured log.
func runClaudeTextGen(args []string) {
	log.Printf("claude one-shot text-generation invocation detected (-p --output-format json)")

	schema := []byte(flagValue(args, "--json-schema"))
	// Claude's OWN validator, not the two-provider union: this schema is
	// Claude's alone (internal/commitmsg and internal/threadtitle each keep a
	// separate Claude constant), and the union's object rules are codex-400
	// rejections that the real `claude -p` accepts. See
	// providerschema.ValidateClaude.
	if len(schema) > 0 {
		rejectSchema("--json-schema", providerschema.ValidateClaude(schema))
	}

	prompt := drainStdin()
	payload, err := cannedStructuredOutput(schema)
	if err != nil {
		// A schema this generator cannot answer is a mock defect, and a silent
		// empty answer would surface as "the model returned nothing" three
		// layers away. Die naming the schema instead.
		log.Fatalf("cannot build canned structured output for --json-schema %s: %v", schema, err)
	}

	w := newLineWriter(os.Stdout)
	w.writeLine(mustJSON(map[string]any{
		"type":              "result",
		"subtype":           "success",
		"is_error":          false,
		"duration_ms":       1,
		"duration_api_ms":   1,
		"num_turns":         1,
		"session_id":        "mock-textgen",
		"total_cost_usd":    0,
		"usage":             map[string]any{"input_tokens": len(prompt) / 4, "output_tokens": 8, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		"result":            string(payload),
		"structured_output": json.RawMessage(payload),
	}), 0, 0)
}

// runCodexTextGen answers `codex exec --ephemeral … --output-last-message P`.
//
// The structured answer travels through the FILE, never stdout:
// textgen.RunCodex reads `--output-last-message` back and hands the raw bytes
// to the caller's decoder. Exit code 0 with an empty file is what a caller
// reports as a decode failure, so the write is the whole contract.
func runCodexTextGen(args []string) {
	log.Printf("codex one-shot text-generation invocation detected (exec --ephemeral)")

	var schema []byte
	if path := flagValue(args, "--output-schema"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read --output-schema %s: %v", path, err)
		}
		// The full union here: the Codex app-server is the strict half of it,
		// and this is the flag it validates.
		rejectSchema("--output-schema", providerschema.Validate(data))
		schema = data
	}

	drainStdin()
	payload, err := cannedStructuredOutput(schema)
	if err != nil {
		log.Fatalf("cannot build canned structured output for --output-schema: %v", err)
	}

	outputPath := flagValue(args, "--output-last-message")
	if outputPath == "" {
		log.Fatalf("codex exec --ephemeral without --output-last-message: nothing could read the answer")
	}
	if err := os.WriteFile(outputPath, payload, 0o600); err != nil {
		log.Fatalf("write --output-last-message %s: %v", outputPath, err)
	}
}

// rejectSchema exits the way a real CLI does when it refuses a
// structured-output schema, naming every broken rule.
func rejectSchema(source string, violations []providerschema.Violation) {
	if len(violations) == 0 {
		return
	}
	for _, violation := range violations {
		log.Printf("%s is not a valid provider schema: %s", source, violation.Error())
	}
	log.Fatalf("%s broke %d provider schema rule(s); a real provider would reject this invocation", source, len(violations))
}

// drainStdin reads the prompt to EOF. The answer is canned, so the bytes are
// unused — but a process that exits without reading leaves the parent's stdin
// copy writing into a closed pipe, and reading is also what the real CLI does.
func drainStdin() string {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Printf("read prompt from stdin: %v (answering anyway)", err)
	}
	return string(data)
}

// cannedStructuredOutput builds a value that satisfies schema, so the answer
// survives the REAL decoder on the other end rather than a decoder written to
// match the mock. An empty schema answers `{}`.
//
// Every declared property is emitted, not just the required ones: a strict
// schema lists them all as required anyway, and a property present where the
// schema allows it can never make the decode fail.
func cannedStructuredOutput(schema []byte) ([]byte, error) {
	if len(strings.TrimSpace(string(schema))) == 0 {
		return []byte(`{}`), nil
	}
	var node map[string]any
	if err := json.Unmarshal(schema, &node); err != nil {
		return nil, fmt.Errorf("schema is not JSON: %w", err)
	}
	value, err := cannedValue(node, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// cannedValue answers one schema node. `name` is the property name the node
// was reached under and drives the canned text, so a `title` reads like a
// thread title and a `subject` like a commit subject.
func cannedValue(node map[string]any, name string) (any, error) {
	if enum, ok := node["enum"].([]any); ok && len(enum) > 0 {
		return enum[0], nil
	}
	switch declaredType(node) {
	case "object":
		properties, _ := node["properties"].(map[string]any)
		out := make(map[string]any, len(properties))
		for _, key := range sortedPropertyNames(properties) {
			child, ok := properties[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("property %q is not a schema object", key)
			}
			value, err := cannedValue(child, key)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case "array":
		items, ok := node["items"].(map[string]any)
		if !ok {
			return []any{}, nil
		}
		// One element rather than none: a `minItems: 1` array is legal in the
		// vocabulary, and an empty array would fail it.
		value, err := cannedValue(items, name)
		if err != nil {
			return nil, err
		}
		return []any{value}, nil
	case "string":
		return cannedText(name), nil
	case "integer", "number":
		return 0, nil
	case "boolean":
		return false, nil
	case "null":
		return nil, nil
	case "":
		return nil, fmt.Errorf("property %q declares no type", name)
	}
	return nil, fmt.Errorf("property %q declares an unsupported type", name)
}

// declaredType picks the type this generator answers. A union
// (`["string","null"]`) is answered with its first NON-null member, because a
// bare null would erase the field for a caller that reads it as a value; a
// union of only null is answered as null.
func declaredType(node map[string]any) string {
	switch declared := node["type"].(type) {
	case string:
		return declared
	case []any:
		fallback := ""
		for _, entry := range declared {
			text, ok := entry.(string)
			if !ok {
				continue
			}
			if text != "null" {
				return text
			}
			fallback = text
		}
		return fallback
	}
	return ""
}

// cannedText is the plausible-answer table. The three schemas textgen actually
// sends today are commit messages (`subject` / `body`), thread titles
// (`title`) and the workflow digest (`whatHappened` / `whatItNeeds`); anything
// else gets a value that names its own field, so a wrong-field bug reads as
// itself in the UI instead of as a generic string.
func cannedText(name string) string {
	switch name {
	case "title":
		return "Mock provider thread title"
	case "subject":
		return "chore: mock provider commit subject"
	case "body":
		return "Generated by ao-mockprovider for the agent test harness."
	case "whatHappened":
		return "The mock provider answered a one-shot text-generation run."
	case "whatItNeeds":
		return "Nothing; this is canned harness output."
	case "":
		return "ao-mockprovider text generation"
	}
	return "mock " + name
}

func sortedPropertyNames(properties map[string]any) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
