package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/transferfiles"
)

// TransferReference reads the provider's CURRENT native path, including after a
// revert changed its rollout ID. It requests no history and performs no model
// turn. The caller releases the provider writer before snapshotting this file.
func (s *Session) TransferReference(ctx context.Context, threadID string) (rollout.TransferReference, error) {
	response, err := s.sendRequest(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return rollout.TransferReference{}, fmt.Errorf("codex: transfer thread/read: %w", err)
	}
	return decodeTransferReference(threadID, response)
}

func decodeTransferReference(threadID string, response json.RawMessage) (rollout.TransferReference, error) {
	var decoded struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		return rollout.TransferReference{}, err
	}
	if decoded.Thread.ID != threadID || decoded.Thread.Path == "" {
		return rollout.TransferReference{}, errors.New("codex: conversation has no durable native transcript")
	}
	return rollout.TransferReference{SessionID: threadID, Path: decoded.Thread.Path}, nil
}

type TransferSnapshotConfig struct {
	Binary  string
	Home    string
	WorkDir string
	Env     map[string]string
}

type TransferSnapshot struct {
	References []rollout.TransferReference
	Files      []transferfiles.Source
}

// ReadTransferSnapshot uses a THREADLESS app-server after the source writer
// has been closed. Resuming a temporary Session here would load the thread and
// drain native queued prompts, executing work just to copy its history. Reuse
// the response-only client and never send thread/start, thread/resume or a turn.
func ReadTransferSnapshot(ctx context.Context, cfg TransferSnapshotConfig, sessionID string) (TransferSnapshot, error) {
	if !filepath.IsAbs(cfg.Home) {
		return TransferSnapshot{}, errors.New("codex transfer: missing injected native home")
	}
	if err := provider.ValidateProbeWorkDir("codex transfer", cfg.WorkDir); err != nil {
		return TransferSnapshot{}, err
	}
	client, err := startOneshotClient(ctx, oneshotSpec{Binary: cfg.Binary, WorkDir: cfg.WorkDir, Env: cfg.Env, ClientName: provider.CodexClientOrigin, Experimental: true, Label: "conversation transfer"})
	if err != nil {
		return TransferSnapshot{}, err
	}
	defer client.close()
	resolve := func(ctx context.Context, id string) (rollout.TransferReference, error) {
		if err := checkTransferQueue(ctx, id, client.request); err != nil {
			return rollout.TransferReference{}, err
		}
		response, err := client.request(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
		if err != nil {
			return rollout.TransferReference{}, err
		}
		return decodeTransferReference(id, response)
	}
	root, err := resolve(ctx, sessionID)
	if err != nil {
		return TransferSnapshot{}, err
	}
	refs, files, err := rollout.TransferGraph(ctx, cfg.Home, root, resolve)
	return TransferSnapshot{References: refs, Files: files}, err
}

func checkTransferQueue(ctx context.Context, threadID string, request func(context.Context, string, any) (json.RawMessage, error)) error {
	response, err := request(ctx, "thread/queue/list", map[string]any{"threadId": threadID, "limit": 1})
	if err != nil {
		var rpc *RPCError
		// Pre-0.148 binaries have no durable native queue. Every other
		// refusal is unknown state, never evidence that the queue is empty.
		if errors.As(err, &rpc) && rpc.Code == -32601 {
			return nil
		}
		return err
	}
	var body struct {
		Data       *[]json.RawMessage `json:"data"`
		NextCursor *string            `json:"nextCursor"`
	}
	if err := json.Unmarshal(response, &body); err != nil || body.Data == nil {
		return errors.New("codex transfer: could not verify the native message queue")
	}
	if len(*body.Data) > 0 || (body.NextCursor != nil && strings.TrimSpace(*body.NextCursor) != "") {
		return errors.New("Finish or remove queued Codex messages before transferring this conversation.")
	}
	return nil
}
