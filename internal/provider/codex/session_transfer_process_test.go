package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A real child process with an intentionally tiny wire vocabulary. It cannot
// execute a provider turn, consult credentials, or use the network. Reusing the
// test binary keeps this cross-platform without Python/shell dependencies.
func TestMain(m *testing.M) {
	if os.Getenv("AO_TEST_CODEX_TRANSFER_SERVER") == "1" {
		if err := runTransferTestServer(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runTransferTestServer() error {
	var paths map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("AO_TEST_TRANSFER_PATHS")), &paths); err != nil {
		return err
	}
	log, err := os.OpenFile(os.Getenv("AO_TEST_TRANSFER_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	decoder, encoder := json.NewDecoder(bufio.NewReader(os.Stdin)), json.NewEncoder(os.Stdout)
	for {
		var request struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := decoder.Decode(&request); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		id, _ := request.Params["threadId"].(string)
		if _, err := fmt.Fprintln(log, request.Method+" "+id); err != nil {
			return err
		}
		if err := log.Sync(); err != nil {
			return err
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]any{"userAgent": "codex/0.153.4"}
		case "initialized":
			continue
		case "account/read":
			if request.Params["refreshToken"] != false {
				return fmt.Errorf("transfer refreshed native credentials")
			}
			var account json.RawMessage = json.RawMessage(os.Getenv("AO_TEST_TRANSFER_ACCOUNT"))
			if !json.Valid(account) {
				return fmt.Errorf("unexpected account read")
			}
			result = account
		case "thread/queue/list":
			if request.Params["limit"] != float64(1) {
				return fmt.Errorf("unbounded native queue read")
			}
			data := []any{}
			if id == os.Getenv("AO_TEST_TRANSFER_QUEUED") {
				data = append(data, map[string]any{"id": "foreign-message"})
			}
			result = map[string]any{"data": data, "nextCursor": nil}
		case "thread/read":
			if request.Params["includeTurns"] != false || paths[id] == "" {
				return fmt.Errorf("unexpected history read")
			}
			result = map[string]any{"thread": map[string]any{"id": id, "path": paths[id]}}
		default:
			return fmt.Errorf("transfer tried forbidden method %s", request.Method)
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return err
		}
	}
}

func TestTransferSnapshotUsesThreadlessProcessForEveryNativeChild(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued child=%v", queued), func(t *testing.T) {
			home, work := t.TempDir(), t.TempDir()
			root, child := uuid.NewString(), uuid.NewString()
			paths := map[string]string{}
			for _, id := range []string{root, child} {
				file := filepath.Join(home, ".codex", "sessions", "2026", "09", "05", "rollout-2026-09-05T12-00-00-"+id+".jsonl")
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				payload := map[string]any{"id": id, "cwd": work, "history_mode": "legacy", "cli_version": "0.153.4"}
				if id == child {
					payload["parent_thread_id"] = root
				}
				meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
				data := string(meta) + "\n"
				if id == root {
					data += `{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"spawn","arguments":"{}"}}` + "\n"
					output, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "spawn", "output": `{"agent_id":"` + child + `"}`}})
					data += string(output) + "\n"
				}
				if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				paths[id] = file
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(paths)
			calls := filepath.Join(t.TempDir(), "calls")
			env := map[string]string{"HOME": home, "USERPROFILE": home, "AO_TEST_CODEX_TRANSFER_SERVER": "1", "AO_TEST_TRANSFER_PATHS": string(encoded), "AO_TEST_TRANSFER_CALLS": calls, "AO_TEST_TRANSFER_QUEUED": ""}
			if queued {
				env["AO_TEST_TRANSFER_QUEUED"] = child
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			snapshot, err := ReadTransferSnapshot(ctx, TransferSnapshotConfig{Binary: binary, Home: filepath.Join(home, ".codex"), WorkDir: work, Env: env}, root)
			if queued {
				if err == nil || !strings.Contains(err.Error(), "queued Codex") {
					t.Fatalf("ignored child queue: %v", err)
				}
			} else if err != nil || len(snapshot.References) != 2 || len(snapshot.Files) != 2 {
				t.Fatalf("native graph: %+v %v", snapshot, err)
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"initialize ", "initialized ", "thread/queue/list " + root, "thread/read " + root, "thread/queue/list " + child}
			if !queued {
				want = append(want, "thread/read "+child)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if !slices.Equal(got, want) {
				t.Fatalf("unexpected provider calls: %q", got)
			}
		})
	}
}

func TestTransferAccountUsesOnlyAccountRead(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		ready        bool
	}{
		{"login", `{"account":{"type":"chatgpt","email":"fixture@example.test"},"requiresOpenaiAuth":true}`, true},
		{"key", `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`, true},
		{"custom endpoint", `{"account":null,"requiresOpenaiAuth":false}`, true},
		{"signed out", `{"account":null,"requiresOpenaiAuth":true}`, false},
		{"unknown", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, work := t.TempDir(), t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(work, "calls")
			err = CheckTransferAccount(context.Background(), ProbeConfig{Binary: binary, WorkDir: work, Env: map[string]string{
				"HOME": home, "USERPROFILE": home, "AO_TEST_CODEX_TRANSFER_SERVER": "1", "AO_TEST_TRANSFER_PATHS": "{}", "AO_TEST_TRANSFER_CALLS": logPath, "AO_TEST_TRANSFER_ACCOUNT": tc.result,
			}})
			if (err == nil) != tc.ready {
				t.Fatalf("readiness: %v, want %v", err, tc.ready)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(calls) != "initialize \ninitialized \naccount/read \n" {
				t.Fatalf("unexpected native operations: %s", calls)
			}
		})
	}
}
