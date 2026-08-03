package triage

import (
	"strings"
	"testing"
)

func TestDetectDevServerURLStartupBanners(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "vite",
			output: "\n  VITE v8.0.1  ready in 312 ms\n\n" +
				"  \x1b[32m➜\x1b[0m  Local:   http://localhost:5173/\n" +
				"  ➜  Network: http://192.168.1.24:5173/\n",
			want: "http://localhost:5173/",
		},
		{
			name:   "next",
			output: "   - Local:        http://localhost:3000\n   - Network:      http://10.0.0.4:3000\n",
			want:   "http://localhost:3000",
		},
		{
			name:   "rails puma",
			output: "* Listening on http://127.0.0.1:3000\n* Listening on http://[::1]:3000\n",
			want:   "http://127.0.0.1:3000",
		},
		{
			name:   "django",
			output: "Starting development server at http://127.0.0.1:8000/\nQuit the server with CONTROL-C.\n",
			want:   "http://127.0.0.1:8000/",
		},
		{
			name:   "flask wildcard bind rewrites to localhost",
			output: " * Running on http://0.0.0.0:5000\nPress CTRL+C to quit\n",
			want:   "http://localhost:5000",
		},
		{
			name:   "ipv6 wildcard bind rewrites to localhost",
			output: "listening on http://[::]:8080\n",
			want:   "http://localhost:8080",
		},
		{
			name:   "ipv6 loopback keeps brackets",
			output: "Server running at http://[::1]:4321/\n",
			want:   "http://[::1]:4321/",
		},
		{
			name:   "ipv6 loopback long form normalizes",
			output: "Server running at http://[0:0:0:0:0:0:0:1]:4321/\n",
			want:   "http://[::1]:4321/",
		},
		{
			name:   "127 subnet loopback",
			output: "bound to http://127.0.0.53:9000/\n",
			want:   "http://127.0.0.53:9000/",
		},
		{
			name:   "https dev server",
			output: "  ➜  Local:   https://localhost:5174/\n",
			want:   "https://localhost:5174/",
		},
		{
			name:   "no port defaults through untouched",
			output: "Serving at http://localhost/\n",
			want:   "http://localhost/",
		},
		{
			name:   "path and query preserved",
			output: "Storybook started on http://localhost:6006/?path=/story/button\n",
			want:   "http://localhost:6006/?path=/story/button",
		},
		{
			name:   "uppercase scheme and host normalize",
			output: "Ready at HTTP://LocalHost:8080/\n",
			want:   "http://localhost:8080/",
		},
		{
			name:   "trailing sentence punctuation trimmed",
			output: "Open http://localhost:1234/admin.\n",
			want:   "http://localhost:1234/admin",
		},
		{
			name:   "first loopback wins over a later one",
			output: "app on http://localhost:3000\napi on http://localhost:4000\n",
			want:   "http://localhost:3000",
		},
		{
			name:   "network url before local url is skipped",
			output: "  ➜  Network: http://192.168.1.24:5173/\n  ➜  Local:   http://localhost:5173/\n",
			want:   "http://localhost:5173/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectDevServerURL(tc.output); got != tc.want {
				t.Fatalf("DetectDevServerURL(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

func TestDetectDevServerURLRejects(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{name: "empty", output: ""},
		{name: "no url", output: "go build ./...\nok\n"},
		{name: "public host", output: "fetching https://registry.npmjs.org/svelte\n"},
		{name: "lan host", output: "  ➜  Network: http://192.168.1.24:5173/\n"},
		{name: "public host that merely mentions localhost", output: "see https://localhost.example.com/docs\n"},
		{name: "non http scheme", output: "connect ws://localhost:5173/hmr\n"},
		{name: "postgres scheme", output: "postgres://localhost:5432/app\n"},
		{name: "scheme embedded in a longer token", output: "xhttp://localhost:3000\n"},
		{
			name:   "js stack frame line and column",
			output: "    at render (http://localhost:3000/assets/index.js:412:19)\n",
		},
		{
			name:   "bare stack frame with line and column",
			output: "TypeError: x is not a function\n    at http://localhost:3000/main.js:12:5\n",
		},
		{name: "markdown link", output: "See [the app](http://localhost:3000) for details.\n"},
		{name: "json string value", output: `{"origin":"http://localhost:3000"}` + "\n"},
		{name: "single quoted", output: "curl -H 'Origin: 'http://localhost:3000\n"},
		{name: "env assignment", output: "VITE_API_URL=http://localhost:8787\n"},
		{name: "angle bracketed", output: "Location: <http://localhost:9000>\n"},
		{name: "port zero", output: "listening on http://localhost:0\n"},
		{name: "port out of range", output: "listening on http://localhost:70000\n"},
		{name: "unbracketed ipv6", output: "listening on http://::1:3000\n"},
		{name: "no host", output: "http:///nowhere\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectDevServerURL(tc.output); got != "" {
				t.Fatalf("DetectDevServerURL(%q) = %q, want empty", tc.output, got)
			}
		})
	}
}

// A rejected candidate must not stop the scan: the stack frame comes
// first, and the real banner after it still has to be found.
func TestDetectDevServerURLSkipsRejectedCandidates(t *testing.T) {
	output := "    at boot (http://localhost:9999/boot.js:3:1)\n" +
		"VITE_PROXY=http://localhost:8888\n" +
		"  ➜  Local:   http://localhost:5173/\n"
	if got := DetectDevServerURL(output); got != "http://localhost:5173/" {
		t.Fatalf("DetectDevServerURL = %q, want the banner URL", got)
	}
}

func TestDetectDevServerURLIgnoresOverlongCandidate(t *testing.T) {
	long := "http://localhost:5173/" + strings.Repeat("a", maxDevServerURLBytes)
	if got := DetectDevServerURL(long + "\n"); got != "" {
		t.Fatalf("DetectDevServerURL = %q, want empty for an overlong candidate", got)
	}
	if got := DetectDevServerURL(long + "\nready on http://localhost:3000\n"); got != "http://localhost:3000" {
		t.Fatalf("DetectDevServerURL = %q, want the following short URL", got)
	}
}

func TestExtractCommandOutputMetaCarriesDevServerURL(t *testing.T) {
	meta := ExtractCommandOutputMeta("  ➜  Local:   http://0.0.0.0:5173/\n", "npm run dev", 0)
	if meta.DevServerURL != "http://localhost:5173/" {
		t.Fatalf("DevServerURL = %q, want the rewritten loopback URL", meta.DevServerURL)
	}
	quiet := ExtractCommandOutputMeta("ok\n", "go build ./...", 0)
	if quiet.DevServerURL != "" {
		t.Fatalf("DevServerURL = %q, want empty", quiet.DevServerURL)
	}
}

func BenchmarkDetectDevServerURLNoMatch(b *testing.B) {
	output := strings.Repeat("go: downloading example.com/mod v1.2.3\n", 4096)
	b.SetBytes(int64(len(output)))
	b.ResetTimer()
	for b.Loop() {
		if DetectDevServerURL(output) != "" {
			b.Fatal("unexpected match")
		}
	}
}
