package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/transferfiles"
	"github.com/klauspost/compress/zstd"
)

// transferRecords shares the native-reference vocabulary between dependency
// discovery and copy. Never recursively replace UUID-shaped strings: prompts,
// tool output, and fork provenance are historical content, not ownership.
type transferRecords struct {
	calls       map[string]bool
	identity    func(string) string
	prefix      func(map[string]any) error
	metadataIDs bool
}

func (r *transferRecords) field(m map[string]any, key string) {
	if value, ok := m[key].(string); ok && looksLikeUUID(value) {
		m[key] = r.identity(value)
	}
}

func (r *transferRecords) fields(m map[string]any, keys ...string) {
	for _, key := range keys {
		r.field(m, key)
	}
}

func (r *transferRecords) array(m map[string]any, key string) {
	values, _ := m[key].([]any)
	for i, value := range values {
		if id, ok := value.(string); ok && looksLikeUUID(id) {
			values[i] = r.identity(id)
		}
	}
}

func (r *transferRecords) agents(m map[string]any) {
	r.fields(m, "sender_thread_id", "receiver_thread_id", "new_thread_id", "agent_thread_id")
	r.array(m, "receiver_thread_ids")
	for _, key := range []string{"receiver_agents", "agent_statuses"} {
		entries, _ := m[key].([]any)
		for _, entry := range entries {
			if agent, ok := entry.(map[string]any); ok {
				r.field(agent, "thread_id")
			}
		}
	}
	for _, key := range []string{"statuses", "agents_states"} {
		if statuses, ok := m[key].(map[string]any); ok {
			mapped := make(map[string]any, len(statuses))
			for id, state := range statuses {
				if looksLikeUUID(id) {
					id = r.identity(id)
				}
				mapped[id] = state
			}
			m[key] = mapped
		}
	}
}

func transferCollabTool(name string) bool {
	switch name {
	case "spawn_agent", "send_input", "wait", "close_agent", "resume_agent":
		return true
	}
	return false
}

func (r *transferRecords) rewrite(line []byte) ([]byte, error) {
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("codex transfer: trailing native record content")
	}
	switch envelope["type"] {
	case "session_meta", "response_item", "event_msg":
	default:
		return line, nil
	}
	p, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, errors.New("codex transfer: invalid native record")
	}
	switch envelope["type"] {
	case "session_meta":
		if r.metadataIDs {
			r.fields(p, "id", "session_id")
		}
		r.field(p, "parent_thread_id")
		if source, ok := p["source"].(map[string]any); ok {
			if subagent, ok := source["subagent"].(map[string]any); ok {
				if spawn, ok := subagent["thread_spawn"].(map[string]any); ok {
					r.field(spawn, "parent_thread_id")
				}
			}
		}
		if base, ok := p["history_base"].(map[string]any); ok && r.prefix != nil {
			if err := r.prefix(base); err != nil {
				return nil, err
			}
		}
	case "response_item":
		callID, _ := p["call_id"].(string)
		switch p["type"] {
		case "function_call":
			name, _ := p["name"].(string)
			if !transferCollabTool(name) {
				return line, nil
			}
			if r.calls == nil {
				r.calls = make(map[string]bool)
			}
			if len(r.calls) >= transferfiles.MaxFiles {
				return nil, errors.New("codex transfer: too many outstanding native tool calls")
			}
			r.calls[callID] = true
			if arguments, ok := p["arguments"].(string); ok {
				if args, ok := transferJSONObject(arguments); ok {
					r.fields(args, "id", "agent_id")
					r.array(args, "ids")
					data, err := json.Marshal(args)
					if err != nil {
						return nil, err
					}
					p["arguments"] = string(data)
				}
			}
		case "function_call_output":
			if !r.calls[callID] {
				return line, nil
			}
			delete(r.calls, callID)
			switch output := p["output"].(type) {
			case string:
				mapped, err := r.collabResult(output)
				if err != nil {
					return nil, err
				}
				p["output"] = mapped
			case []any:
				// The native Responses payload also permits multimodal content items.
				// Only input_text can carry the collaboration result. Images, audio,
				// encrypted content and unknown future items remain opaque.
				for _, value := range output {
					item, ok := value.(map[string]any)
					if !ok || item["type"] != "input_text" {
						continue
					}
					text, ok := item["text"].(string)
					if !ok {
						continue
					}
					mapped, err := r.collabResult(text)
					if err != nil {
						return nil, err
					}
					item["text"] = mapped
				}
			}
		default:
			return line, nil
		}
	case "event_msg":
		kind, _ := p["type"].(string)
		if strings.HasPrefix(kind, "collab_") || kind == "sub_agent_activity" {
			r.agents(p)
		} else if kind == "item_completed" {
			item, _ := p["item"].(map[string]any)
			kind, _ := item["type"].(string)
			if !strings.EqualFold(kind, "CollabAgentToolCall") {
				return line, nil
			}
			r.agents(item)
		} else {
			return line, nil
		}
	default:
		return line, nil
	}
	return json.Marshal(envelope)
}

func transferJSONObject(text string) (map[string]any, bool) {
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil {
		return nil, false
	}
	if !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return nil, false
	}
	return result, true
}

func (r *transferRecords) collabResult(text string) (string, error) {
	result, ok := transferJSONObject(text)
	if !ok {
		return text, nil
	}
	r.field(result, "agent_id")
	if statuses, ok := result["status"].(map[string]any); ok {
		mapped := make(map[string]any, len(statuses))
		for id, value := range statuses {
			if looksLikeUUID(id) {
				id = r.identity(id)
			}
			mapped[id] = value
		}
		result["status"] = mapped
	}
	data, err := json.Marshal(result)
	return string(data), err
}

// walkTransferRecords refuses partial/oversized records instead of silently
// dropping native history. Limits apply to decoded bytes, including zstd input.
func walkTransferRecords(ctx context.Context, source transferfiles.Source, visit func(scanLine) error) error {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(source.Path))
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(source.Path, ".zst") {
		decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return err
		}
		defer decoder.Close()
		reader = decoder
	}
	scan := newScanner(io.LimitReader(reader, transferfiles.MaxFileBytes+1), 0, DefaultMaxLineBytes, scanBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := scan.next()
		if errors.Is(err, io.EOF) {
			if len(scan.buf) != 0 {
				return errors.New("codex transfer: incomplete native record")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if line.Oversized || line.Next > transferfiles.MaxFileBytes {
			return errors.New("codex transfer: native history exceeds the transfer limit")
		}
		if err := visit(line); err != nil {
			return err
		}
	}
}
