package app

import (
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/errorsx"
)

func TestRemoteJobLabel(t *testing.T) {
	for _, test := range []struct {
		name, label string
		request     RemoteCommandRequest
		want        string
		invalid     bool
	}{
		{name: "trimmed description", label: "  Windows integration tests  ", want: "Windows integration tests"},
		{name: "unicode limit", label: strings.Repeat("界", 120), want: strings.Repeat("界", 120)},
		{name: "over limit", label: strings.Repeat("界", 121), invalid: true},
		{name: "newline", label: "build\ncomplete", invalid: true},
		{name: "leading control", label: "\tbuild", invalid: true},
		{name: "terminal control", label: "build\x1b[2J", invalid: true},
		{name: "unicode line separator", label: "build\u2028complete", invalid: true},
		{name: "invalid encoding", label: "build\xff", invalid: true},
		{name: "command fallback", request: RemoteCommandRequest{Argv: []string{"go", "test", "./..."}}, want: "go test ./..."},
		{name: "argument boundaries", request: RemoteCommandRequest{Argv: []string{"runner", "two words", "", "line\nnext", `C:\project`}}, want: `runner "two words" "" "line\nnext" "C:\\project"`},
		{name: "whitespace fallback", label: "   ", request: RemoteCommandRequest{Argv: []string{"build"}}, want: "build"},
		{name: "script fallback", request: RemoteCommandRequest{Script: "private script body", Interpreter: []string{"python3", "-u"}}, want: "python3 -u script"},
		{name: "unicode truncation", request: RemoteCommandRequest{Argv: []string{strings.Repeat("界", 125)}}, want: strings.Repeat("界", 119) + "…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := remoteJobLabel(test.label, test.request)
			if test.invalid {
				code, _, _ := errorsx.PublicDetails(err)
				if code != "remote_invalid_label" {
					t.Fatalf("invalid label accepted or opaque error: %q %v", got, err)
				}
				return
			}
			if err != nil || got != test.want || !utf8.ValidString(got) || utf8.RuneCountInString(got) > remoteJobLabelMaxRunes {
				t.Fatalf("label = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
