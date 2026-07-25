package wsllauncher

import (
	"reflect"
	"testing"
)

func TestAppendWSLENV(t *testing.T) {
	tests := []struct {
		name  string
		env   []string
		names []string
		want  []string
	}{
		{
			name:  "adds WSLENV when variable is set",
			env:   []string{"AGENT_OVERFLOW_PPROF=1"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"AGENT_OVERFLOW_PPROF=1", "WSLENV=AGENT_OVERFLOW_PPROF"},
		},
		{
			name:  "skips variables not present in env",
			env:   []string{"PATH=/usr/bin"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"PATH=/usr/bin"},
		},
		{
			name:  "appends to existing WSLENV",
			env:   []string{"WSLENV=FOO/p", "AGENT_OVERFLOW_PPROF=1"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"WSLENV=FOO/p:AGENT_OVERFLOW_PPROF", "AGENT_OVERFLOW_PPROF=1"},
		},
		{
			name:  "does not duplicate an already-listed name",
			env:   []string{"WSLENV=AGENT_OVERFLOW_PPROF", "AGENT_OVERFLOW_PPROF=1"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"WSLENV=AGENT_OVERFLOW_PPROF", "AGENT_OVERFLOW_PPROF=1"},
		},
		{
			name:  "flagged existing entry counts as listed",
			env:   []string{"WSLENV=AGENT_OVERFLOW_PPROF/u", "AGENT_OVERFLOW_PPROF=1"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"WSLENV=AGENT_OVERFLOW_PPROF/u", "AGENT_OVERFLOW_PPROF=1"},
		},
		{
			name:  "multiple names in one call",
			env:   []string{"A=1", "B=2"},
			names: []string{"A", "B"},
			want:  []string{"A=1", "B=2", "WSLENV=A:B"},
		},
		{
			name:  "case-insensitive variable key match",
			env:   []string{"agent_overflow_pprof=1"},
			names: []string{"AGENT_OVERFLOW_PPROF"},
			want:  []string{"agent_overflow_pprof=1", "WSLENV=AGENT_OVERFLOW_PPROF"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AppendWSLENV(tt.env, tt.names...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("AppendWSLENV(%v, %v)\n got %v\nwant %v", tt.env, tt.names, got, tt.want)
			}
		})
	}
}

func TestAppendWSLENVDoesNotMutateInput(t *testing.T) {
	env := []string{"WSLENV=FOO", "BAR=1"}
	AppendWSLENV(env, "BAR")
	if env[0] != "WSLENV=FOO" {
		t.Fatalf("input slice mutated: %v", env)
	}
}
