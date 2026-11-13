package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// system/session_writes_lost (2.1.317+) says the CLI could not save its
// transcript, ~/.claude.json, or both, because the disk was full or over
// quota. It is a non-fatal error on the thread.
func TestParseSystem_SessionWritesLostIsAnError(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		contains    []string
		notContains []string
		meta        map[string]any
	}{
		{
			name:     "transcript and config on a full disk",
			line:     `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["transcript","global_config"],"transcript_session_ids":["s-1"],"uuid":"u-1","session_id":"s-1"}`,
			contains: []string{"transcript", "~/.claude.json", "the disk is full", "Resuming"},
			meta: map[string]any{
				"kind":                 "session_writes_lost",
				"errorCode":            "ENOSPC",
				"lost":                 []any{"transcript", "global_config"},
				"transcriptSessionIds": []any{"s-1"},
			},
		},
		{
			name:        "transcript only over quota",
			line:        `{"type":"system","subtype":"session_writes_lost","error_code":"EDQUOT","lost":["transcript"],"transcript_session_ids":["s-0","s-1"],"uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"transcript", "the disk quota is used up", "Resuming"},
			notContains: []string{"~/.claude.json"},
			meta: map[string]any{
				"kind":                 "session_writes_lost",
				"errorCode":            "EDQUOT",
				"lost":                 []any{"transcript"},
				"transcriptSessionIds": []any{"s-0", "s-1"},
			},
		},
		{
			name:        "config only",
			line:        `{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["global_config"],"uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"~/.claude.json", "the disk is full"},
			notContains: []string{"transcript", "Resuming"},
			meta: map[string]any{
				"kind":      "session_writes_lost",
				"errorCode": "ENOSPC",
				"lost":      []any{"global_config"},
			},
		},
		{
			name:        "absent error code and lost list",
			line:        `{"type":"system","subtype":"session_writes_lost","uuid":"u-1","session_id":"s-1"}`,
			contains:    []string{"could not save", "the disk is full or over quota"},
			notContains: []string{"Resuming"},
			meta:        map[string]any{"kind": "session_writes_lost"},
		},
		{
			name:     "an unknown code and an unknown lost value",
			line:     `{"type":"system","subtype":"session_writes_lost","error_code":"EIO","lost":["transcript","plugin_cache"],"uuid":"u-1","session_id":"s-1"}`,
			contains: []string{"transcript", "a disk error (EIO)"},
			meta: map[string]any{
				"kind":      "session_writes_lost",
				"errorCode": "EIO",
				"lost":      []any{"transcript", "plugin_cache"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := ParseLine(testThread, []byte(tt.line))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 1 || events[0].Kind != provider.EventError {
				t.Fatalf("events = %+v, want one EventError", events)
			}
			for _, want := range tt.contains {
				if !strings.Contains(events[0].Content, want) {
					t.Fatalf("Content = %q, want it to contain %q", events[0].Content, want)
				}
			}
			for _, unwanted := range tt.notContains {
				if strings.Contains(events[0].Content, unwanted) {
					t.Fatalf("Content = %q, must not contain %q", events[0].Content, unwanted)
				}
			}
			var meta map[string]any
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if len(meta) != len(tt.meta) {
				t.Fatalf("meta = %+v, want %+v", meta, tt.meta)
			}
			for key, want := range tt.meta {
				got, _ := json.Marshal(meta[key])
				wantJSON, _ := json.Marshal(want)
				if string(got) != string(wantJSON) {
					t.Fatalf("meta[%q] = %s, want %s", key, got, wantJSON)
				}
			}
			// Triage reads a top-level `fatal` as "the provider process is
			// gone" and a top-level `error` string as an SDK error enum.
			if _, present := meta["fatal"]; present {
				t.Fatalf("meta must not claim fatal: %+v", meta)
			}
			if _, present := meta["error"]; present {
				t.Fatalf("meta must not carry a top-level error enum: %+v", meta)
			}

			// A second delivery composes the same row text, which is what
			// lets triage's matching-row check drop it.
			again, err := ParseLine(testThread, []byte(tt.line))
			if err != nil || len(again) != 1 || again[0].Content != events[0].Content {
				t.Fatalf("second parse = %+v, %v; want the same content", again, err)
			}
		})
	}
}
