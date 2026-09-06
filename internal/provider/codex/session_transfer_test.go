package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestTransferQueueNeedsEvidenceOfAnEmptyNativeQueue(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		requestErr error
		allowed    bool
	}{
		{name: "empty", body: `{"data":[],"nextCursor":null}`, allowed: true},
		{name: "old cli", requestErr: &RPCError{Code: -32601, Message: "method not found"}, allowed: true},
		{name: "queued", body: `{"data":[{"id":"message"}]}`},
		{name: "remaining page", body: `{"data":[],"nextCursor":"more"}`},
		{name: "unknown shape", body: `{}`},
		{name: "null", body: `{"data":null}`},
		{name: "read failed", requestErr: errors.New("database busy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTransferQueue(context.Background(), "native-session", func(ctx context.Context, method string, args any) (json.RawMessage, error) {
				if method != "thread/queue/list" {
					t.Fatalf("unexpected native mutation %s", method)
				}
				params := args.(map[string]any)
				if params["threadId"] != "native-session" || params["limit"] != 1 {
					t.Fatalf("unbounded or unrelated queue read: %+v", params)
				}
				return json.RawMessage(tc.body), tc.requestErr
			})
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed %v: %v", tc.allowed, err)
			}
		})
	}
}
