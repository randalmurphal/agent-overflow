package notify

import (
	"reflect"
	"strings"
	"testing"
)

func TestTargetRoundTrip(t *testing.T) {
	for _, want := range []Target{
		{Kind: "thread", ThreadID: "thread-123"},
		{Kind: "workflow-item", WorkItemID: "item-123"},
		{Kind: "workflow-triage-agent", ProjectID: "project-123"},
		{Kind: "none"},
	} {
		t.Run(want.Kind, func(t *testing.T) {
			data, err := TargetToMap(want)
			if err != nil {
				t.Fatalf("encode target: %v", err)
			}
			got, err := TargetFromMap(data)
			if err != nil {
				t.Fatalf("decode target: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip = %#v, want %#v", got, want)
			}
		})
	}
}

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		name   string
		target Target
	}{
		{name: "missing kind", target: Target{}},
		{name: "thread missing id", target: Target{Kind: "thread"}},
		{name: "none with id", target: Target{Kind: "none", ThreadID: "unexpected"}},
		{name: "oversized thread id", target: Target{Kind: "thread", ThreadID: strings.Repeat("x", MaxThreadIDBytes+1)}},
		{name: "thread with item", target: Target{Kind: "thread", ThreadID: "thread", WorkItemID: "item"}},
		{name: "item missing id", target: Target{Kind: "workflow-item"}},
		{name: "item with project", target: Target{Kind: "workflow-item", WorkItemID: "item", ProjectID: "project"}},
		{name: "oversized item id", target: Target{Kind: "workflow-item", WorkItemID: strings.Repeat("x", MaxWorkItemIDBytes+1)}},
		{name: "triage missing project", target: Target{Kind: "workflow-triage-agent"}},
		{name: "triage with thread", target: Target{Kind: "workflow-triage-agent", ProjectID: "project", ThreadID: "thread"}},
		{name: "oversized project id", target: Target{Kind: "workflow-triage-agent", ProjectID: strings.Repeat("x", MaxProjectIDBytes+1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTarget(tt.target); err == nil {
				t.Fatalf("validate %#v unexpectedly succeeded", tt.target)
			}
		})
	}
}
