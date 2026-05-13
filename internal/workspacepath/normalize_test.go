package workspacepath

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRelative(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "simple relative path",
			input: "plans/ship-it.md",
			want:  filepath.Join("plans", "ship-it.md"),
		},
		{
			name:  "redundant separators collapsed",
			input: "plans//ship-it.md",
			want:  filepath.Join("plans", "ship-it.md"),
		},
		{
			name:  "trims surrounding whitespace",
			input: "  notes.txt  ",
			want:  "notes.txt",
		},
		{
			name:  "inner ..  resolved to safe path",
			input: "plans/draft/../ship-it.md",
			want:  filepath.Join("plans", "ship-it.md"),
		},
		{
			name:    "empty rejected",
			input:   "",
			wantErr: "workspace path is required",
		},
		{
			name:    "whitespace only rejected",
			input:   "   ",
			wantErr: "workspace path is required",
		},
		{
			name:    "absolute path rejected",
			input:   string(filepath.Separator) + "etc" + string(filepath.Separator) + "passwd",
			wantErr: "must be relative",
		},
		{
			name:    "dot rejected",
			input:   ".",
			wantErr: "must stay within the workspace root",
		},
		{
			name:    "dot-dot rejected",
			input:   "..",
			wantErr: "must stay within the workspace root",
		},
		{
			name:    "parent escape rejected",
			input:   ".." + string(filepath.Separator) + "secret.txt",
			wantErr: "must stay within the workspace root",
		},
		{
			name:    "inner parent escape rejected",
			input:   "plans/../../secret.txt",
			wantErr: "must stay within the workspace root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRelative(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeRelative(%q) returned (%q, nil), wanted error containing %q", tc.input, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeRelative(%q) error = %q, want substring %q", tc.input, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRelative(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeRelative(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
