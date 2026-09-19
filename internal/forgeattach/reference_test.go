package forgeattach

import (
	"strings"
	"testing"
)

func TestParseReferenceGitHub(t *testing.T) {
	cases := []struct {
		name     string
		href     string
		request  string
		filename string
	}{
		{
			name:     "user-attachments asset",
			href:     "https://github.com/user-attachments/assets/1f0c2c2e-1111-2222-3333-444455556666",
			request:  "https://github.com/user-attachments/assets/1f0c2c2e-1111-2222-3333-444455556666",
			filename: "1f0c2c2e-1111-2222-3333-444455556666",
		},
		{
			name:     "user-attachments file keeps its name",
			href:     "https://github.com/user-attachments/files/18234567/trace%20log.txt",
			request:  "https://github.com/user-attachments/files/18234567/trace%20log.txt",
			filename: "trace log.txt",
		},
		{
			name:     "repo-scoped asset",
			href:     "https://github.com/acme/widget/assets/4242/9d0f1a2b-1111-2222-3333-444455556666",
			request:  "https://github.com/acme/widget/assets/4242/9d0f1a2b-1111-2222-3333-444455556666",
			filename: "9d0f1a2b-1111-2222-3333-444455556666",
		},
		{
			name:     "repo-scoped file",
			href:     "https://github.com/acme/widget/files/991/report.pdf",
			request:  "https://github.com/acme/widget/files/991/report.pdf",
			filename: "report.pdf",
		},
		{
			name:     "http is normalized to https",
			href:     "http://github.com/user-attachments/assets/abc",
			request:  "https://github.com/user-attachments/assets/abc",
			filename: "abc",
		},
		{
			name:     "signed redirect host keeps its query",
			href:     "https://private-user-images.githubusercontent.com/42/clip.mp4?jwt=eyJhbGci&X-Amz=1",
			request:  "https://private-user-images.githubusercontent.com/42/clip.mp4?jwt=eyJhbGci&X-Amz=1",
			filename: "clip.mp4",
		},
		{
			name:     "surrounding whitespace is trimmed",
			href:     "  https://github.com/user-attachments/assets/abc\n",
			request:  "https://github.com/user-attachments/assets/abc",
			filename: "abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseReference("github", "acme/widget", tc.href)
			if err != nil {
				t.Fatalf("ParseReference(%q) returned error: %v", tc.href, err)
			}
			if target.Forge != "github" {
				t.Errorf("Forge = %q, want github", target.Forge)
			}
			if target.Request != tc.request {
				t.Errorf("Request = %q, want %q", target.Request, tc.request)
			}
			if target.Filename != tc.filename {
				t.Errorf("Filename = %q, want %q", target.Filename, tc.filename)
			}
		})
	}
}

func TestParseReferenceGitLab(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name     string
		href     string
		project  string
		request  string
		filename string
	}{
		{
			name:     "project-relative upload",
			href:     "/uploads/" + secret + "/screenshot.png",
			project:  "group/sub/widget",
			request:  "projects/group%2Fsub%2Fwidget/uploads/" + secret + "/screenshot.png",
			filename: "screenshot.png",
		},
		{
			name:     "numeric project id form",
			href:     "/-/project/1234/uploads/" + secret + "/diagram.svg",
			project:  "group/widget",
			request:  "projects/1234/uploads/" + secret + "/diagram.svg",
			filename: "diagram.svg",
		},
		{
			name:     "absolute repository URL",
			href:     "https://gitlab.example.com/group/sub/widget/uploads/" + secret + "/clip.mp4",
			project:  "other/repo",
			request:  "projects/group%2Fsub%2Fwidget/uploads/" + secret + "/clip.mp4",
			filename: "clip.mp4",
		},
		{
			name:     "absolute URL with the /-/ separator",
			href:     "https://gitlab.example.com/group/widget/-/uploads/" + secret + "/clip.mp4",
			project:  "other/repo",
			request:  "projects/group%2Fwidget/uploads/" + secret + "/clip.mp4",
			filename: "clip.mp4",
		},
		{
			name:     "absolute numeric project id",
			href:     "https://gitlab.example.com/-/project/77/uploads/" + secret + "/a.png",
			project:  "other/repo",
			request:  "projects/77/uploads/" + secret + "/a.png",
			filename: "a.png",
		},
		{
			name:    "escaped name keeps its wire spelling and decodes for display",
			href:    "/uploads/" + secret + "/Screen%20Shot%20%281%29.png",
			project: "group/widget",
			request: "projects/group%2Fwidget/uploads/" + secret +
				"/Screen%20Shot%20%281%29.png",
			filename: "Screen Shot (1).png",
		},
		{
			name:     "a repository called uploads does not shift the match",
			href:     "https://gitlab.example.com/group/uploads/uploads/" + secret + "/a.png",
			project:  "other/repo",
			request:  "projects/group%2Fuploads/uploads/" + secret + "/a.png",
			filename: "a.png",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseReference("gitlab", tc.project, tc.href)
			if err != nil {
				t.Fatalf("ParseReference(%q) returned error: %v", tc.href, err)
			}
			if target.Forge != "gitlab" {
				t.Errorf("Forge = %q, want gitlab", target.Forge)
			}
			if target.Request != tc.request {
				t.Errorf("Request = %q, want %q", target.Request, tc.request)
			}
			if target.Filename != tc.filename {
				t.Errorf("Filename = %q, want %q", target.Filename, tc.filename)
			}
		})
	}
}

func TestParseReferenceRefusals(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		forge   string
		project string
		href    string
	}{
		{name: "empty", forge: "github", href: "   "},
		{name: "unknown forge", forge: "gitea", href: "https://github.com/user-attachments/assets/a"},
		{name: "foreign host", forge: "github", href: "https://evil.example.com/user-attachments/assets/a"},
		{name: "host with a port", forge: "github", href: "https://github.com:8443/user-attachments/assets/a"},
		{name: "userinfo in the authority", forge: "github", href: "https://u:p@github.com/user-attachments/assets/a"},
		{name: "not an attachment path", forge: "github", href: "https://github.com/acme/widget/pull/7"},
		{name: "file scheme", forge: "github", href: "file:///etc/passwd"},
		{name: "traversal segment", forge: "github", href: "https://github.com/user-attachments/files/1/.."},
		{name: "encoded traversal", forge: "github", href: "https://github.com/user-attachments/files/1/%2e%2e"},
		{name: "encoded separator in the name", forge: "github", href: "https://github.com/user-attachments/files/1/a%2Fb"},
		{name: "control character", forge: "github", href: "https://github.com/user-attachments/assets/a\x01b"},
		{name: "gitlab short secret", forge: "gitlab", project: "g/r", href: "/uploads/0123456789abcdef/a.png"},
		{name: "gitlab uppercase secret", forge: "gitlab", project: "g/r", href: "/uploads/0123456789ABCDEF0123456789ABCDEF/a.png"},
		{name: "gitlab missing name", forge: "gitlab", project: "g/r", href: "/uploads/" + secret},
		{name: "gitlab extra trailing segment", forge: "gitlab", project: "g/r", href: "/uploads/" + secret + "/a/b.png"},
		{name: "gitlab bare relative path", forge: "gitlab", project: "g/r", href: "uploads/" + secret + "/a.png"},
		{name: "gitlab relative with no project", forge: "gitlab", project: "", href: "/uploads/" + secret + "/a.png"},
		{name: "gitlab absolute one-segment project", forge: "gitlab", project: "g/r", href: "https://gitlab.example.com/widget/uploads/" + secret + "/a.png"},
		{name: "gitlab non-http scheme", forge: "gitlab", project: "g/r", href: "ftp://gitlab.example.com/g/r/uploads/" + secret + "/a.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseReference(tc.forge, tc.project, tc.href)
			if err == nil {
				t.Fatalf("ParseReference(%q) = %+v, want an error", tc.href, target)
			}
			// The message reaches a review pane, so it must not echo a
			// reference that can carry a signed query or an upload secret.
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q leaks the upload secret", err)
			}
		})
	}
}

func TestCacheKeySeparatesReferences(t *testing.T) {
	a := CacheKey("gitlab", "g/r", 7, "/uploads/x/a.png")
	b := CacheKey("gitlab", "g/r", 8, "/uploads/x/a.png")
	c := CacheKey("gitlab", "g/r2", 7, "/uploads/x/a.png")
	if a == b || a == c {
		t.Fatalf("CacheKey collides across PRs: %q %q %q", a, b, c)
	}
	// Trimmed the same way ParseReference trims, so the key a hit is
	// stored under is the key the next render looks up.
	if CacheKey("gitlab", "g/r", 7, "  /uploads/x/a.png\n") != a {
		t.Fatal("CacheKey does not trim the href the way ParseReference does")
	}
}
