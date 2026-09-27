package prthread

import "testing"

func TestForgeNoun(t *testing.T) {
	cases := []struct{ id, want string }{
		{"gitlab", "MR"},
		{"github", "PR"},
		{"", "PR"},
		{"bitbucket", "PR"},
	}
	for _, tc := range cases {
		if got := ForgeNoun(tc.id); got != tc.want {
			t.Errorf("ForgeNoun(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestForgeNounLong(t *testing.T) {
	cases := []struct{ id, want string }{
		{"gitlab", "merge request"},
		{"github", "pull request"},
		{"", "pull request"},
		{"bitbucket", "pull request"},
	}
	for _, tc := range cases {
		if got := ForgeNounLong(tc.id); got != tc.want {
			t.Errorf("ForgeNounLong(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestFenceForContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "```"},
		{"no backticks", "hello world", "```"},
		{"single backtick", "`x`", "```"},
		{"double backtick", "``x``", "```"},
		{"triple backtick", "```", "````"},
		{"four backticks", "````", "`````"},
		{"backtick runs split by content", "``` foo ```", "````"},
		{"longest run wins", "``\nhello\n````\nworld\n```", "`````"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FenceForContent(tt.content)
			if got != tt.want {
				t.Fatalf("FenceForContent(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}
