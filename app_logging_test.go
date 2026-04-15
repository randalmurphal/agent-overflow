package main

import "testing"

func TestProviderEventLoggingEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty", value: "", want: false},
		{name: "provider", value: "provider", want: true},
		{name: "all", value: "all", want: true},
		{name: "multiple includes provider", value: "rpc,provider,background", want: true},
		{name: "multiple excludes provider", value: "rpc,background", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerEventLoggingEnabled(tt.value); got != tt.want {
				t.Fatalf("providerEventLoggingEnabled(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
