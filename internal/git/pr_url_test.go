package git

import "testing"

func TestParsePRURLCreatePRFormats(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want PRReference
	}{
		{
			name: "github",
			url:  "https://github.com/owner/repo/pull/9",
			want: PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9},
		},
		{
			name: "gitlab subgroup",
			url:  "https://gitlab.com/group/sub/repo/-/merge_requests/12",
			want: PRReference{Forge: "gitlab", Namespace: "group/sub", Repo: "repo", Number: 12},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParsePRURL(test.url)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ParsePRURL(%q) = %+v, want %+v", test.url, got, test.want)
			}
		})
	}
}

func TestParsePRURLRejectsMalformedOrUnsupportedValues(t *testing.T) {
	for _, value := range []string{
		"owner/repo/pull/9",
		"https://github.com/owner/repo/pull/0",
		"https://github.com/owner/repo/issues/9",
		"https://gitlab.com/group/repo/-/merge_requests/not-a-number",
		"https://gitlab.com/repo/-/merge_requests/1",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParsePRURL(value); err == nil {
				t.Fatalf("ParsePRURL(%q) returned nil error", value)
			}
		})
	}
}
