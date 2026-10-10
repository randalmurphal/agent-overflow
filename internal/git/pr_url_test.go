package git

import "testing"

func TestParsePRURLCreatePRFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		want PRReference
	}{
		{
			name: "github",
			url:  "https://github.com/owner/repo/pull/9",
			want: PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9},
		},
		{
			name: "github enterprise host with port, case folded",
			url:  "https://GHE.Corp.example:8443/owner/repo/pull/9",
			want: PRReference{Forge: "github", Host: "ghe.corp.example:8443", Namespace: "owner", Repo: "repo", Number: 9},
		},
		{
			name: "gitlab subgroup",
			url:  "https://gitlab.com/group/sub/repo/-/merge_requests/12",
			want: PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "group/sub", Repo: "repo", Number: 12},
		},
		{
			name: "self-hosted gitlab",
			url:  "https://gitlab.example.com/group/repo/-/merge_requests/3",
			want: PRReference{Forge: "gitlab", Host: "gitlab.example.com", Namespace: "group", Repo: "repo", Number: 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	for _, value := range []string{
		"owner/repo/pull/9",
		"https://github.com/owner/repo/pull/0",
		"https://github.com/owner/repo/issues/9",
		"https://gitlab.com/group/repo/-/merge_requests/not-a-number",
		"https://gitlab.com/repo/-/merge_requests/1",
		"https://github.com:70000/owner/repo/pull/9",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParsePRURL(value); err == nil {
				t.Fatalf("ParsePRURL(%q) returned nil error", value)
			}
		})
	}
}

// TestParsePRURLSpellsTheHostAsURLHost: the host matches what the
// frontend's URL.host gives for the same URL (prReference.test.ts holds the
// mirror cases), so a default or empty port does not split one PR into two
// keys.
func TestParsePRURLSpellsTheHostAsURLHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url, host, key string
	}{
		{"https://github.com:443/owner/repo/pull/9", "github.com", "github:owner/repo:9"},
		{"http://github.com:80/owner/repo/pull/9", "github.com", "github:owner/repo:9"},
		{"https://github.com:/owner/repo/pull/9", "github.com", "github:owner/repo:9"},
		{"https://GitLab.com:0443/group/repo/-/merge_requests/3", "gitlab.com", "gitlab:group/repo:3"},
		{"http://ghe.example:443/owner/repo/pull/9", "ghe.example:443", "github@ghe.example:443:owner/repo:9"},
		{"https://ghe.example:80/owner/repo/pull/9", "ghe.example:80", "github@ghe.example:80:owner/repo:9"},
		{"https://ghe.example:08443/owner/repo/pull/9", "ghe.example:8443", "github@ghe.example:8443:owner/repo:9"},
		{"https://[::1]:443/owner/repo/pull/9", "[::1]", "github@[::1]:owner/repo:9"},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePRURL(test.url)
			if err != nil {
				t.Fatal(err)
			}
			if got.Host != test.host || got.Key() != test.key {
				t.Fatalf("ParsePRURL(%q) host %q key %q, want %q %q", test.url, got.Host, got.Key(), test.host, test.key)
			}
		})
	}
}
