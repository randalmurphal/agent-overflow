package externalurl

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestValidateAcceptsHTTPAndHTTPS(t *testing.T) {
	cases := []string{
		"https://example.com/path?q=1#frag",
		"http://localhost:3000/callback",
		"  https://example.com/trimmed  ",
	}
	for _, in := range cases {
		got, err := Validate(in)
		if err != nil {
			t.Fatalf("Validate(%q) unexpected error: %v", in, err)
		}
		if !strings.HasPrefix(got, "http://") && !strings.HasPrefix(got, "https://") {
			t.Fatalf("Validate(%q) = %q, want http(s) URL", in, got)
		}
	}
}

func TestValidateRejectsUnsupportedOrHostlessURLs(t *testing.T) {
	cases := []string{
		"",
		"javascript:alert(1)",
		"mailto:person@example.com",
		"https:///missing-host",
		"/relative/path",
	}
	for _, in := range cases {
		if got, err := Validate(in); err == nil {
			t.Fatalf("Validate(%q) = %q, want error", in, got)
		}
	}
}

func TestValidateErrorDoesNotExposeMalformedURL(t *testing.T) {
	_, err := Validate("https://example.com/%zz?token=secret-token")
	if err == nil {
		t.Fatal("Validate returned nil error, want malformed URL error")
	}
	errorText := err.Error()
	if strings.Contains(errorText, "secret-token") || strings.Contains(errorText, "%zz") {
		t.Fatalf("Validate error %q exposed raw URL content", errorText)
	}
}

func TestCommandCandidatesUseWindowsInteropForWSL(t *testing.T) {
	got := commandCandidates("linux", true, "https://example.com")
	if len(got) != 1 {
		t.Fatalf("commandCandidates returned %d commands, want 1", len(got))
	}
	if got[0].Name != "rundll32.exe" {
		t.Fatalf("WSL opener = %q, want rundll32.exe", got[0].Name)
	}
	wantArgs := []string{"url.dll,FileProtocolHandler", "https://example.com"}
	if strings.Join(got[0].Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("WSL args = %#v, want %#v", got[0].Args, wantArgs)
	}
}

func TestOpenUsesFirstAvailableCandidate(t *testing.T) {
	var started []Command
	candidates := []Command{
		{Name: "xdg-open", Args: []string{"https://example.com"}},
		{Name: "x-www-browser", Args: []string{"https://example.com"}},
	}

	err := open(
		t.Context(),
		candidates,
		func(name string) (string, error) {
			if name == "xdg-open" {
				return "", execNotFound(name)
			}
			return "/usr/bin/" + name, nil
		},
		func(_ context.Context, command Command) error {
			started = append(started, command)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("open returned error: %v", err)
	}
	if len(started) != 1 || started[0].Name != "x-www-browser" {
		t.Fatalf("started = %#v, want only x-www-browser", started)
	}
}

func execNotFound(name string) error {
	return &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func TestOpenReportsNoOpenerWhenEveryCandidateIsMissing(t *testing.T) {
	err := open(
		t.Context(),
		[]Command{{Name: "xdg-open"}, {Name: "x-www-browser"}},
		func(name string) (string, error) { return "", execNotFound(name) },
		func(context.Context, Command) error {
			t.Fatal("start called with no opener on PATH")
			return nil
		},
	)
	if !errors.Is(err, ErrNoOpener) {
		t.Fatalf("open error = %v, want ErrNoOpener", err)
	}
	// The names are what makes the sentence diagnosable; the sentinel alone
	// says nothing about which candidates were tried.
	if !strings.Contains(err.Error(), "xdg-open") || !strings.Contains(err.Error(), "x-www-browser") {
		t.Fatalf("open error %q does not name the candidates it tried", err)
	}
}

func TestOpenReportsNoOpenerWhenThePlatformNamesNone(t *testing.T) {
	err := open(
		t.Context(),
		nil,
		func(string) (string, error) { t.Fatal("lookup called with no candidates"); return "", nil },
		func(context.Context, Command) error { t.Fatal("start called with no candidates"); return nil },
	)
	if !errors.Is(err, ErrNoOpener) {
		t.Fatalf("open error = %v, want ErrNoOpener", err)
	}
}

// A candidate that exists and fails is NOT the no-opener answer: something is
// installed, so a caller must not degrade to "this host cannot open links".
func TestOpenDoesNotReportNoOpenerWhenAnInstalledOpenerFails(t *testing.T) {
	err := open(
		t.Context(),
		[]Command{{Name: "xdg-open"}, {Name: "x-www-browser"}},
		func(name string) (string, error) {
			if name == "xdg-open" {
				return "", execNotFound(name)
			}
			return "/usr/bin/" + name, nil
		},
		func(context.Context, Command) error { return errors.New("display refused the launch") },
	)
	if err == nil {
		t.Fatal("open returned nil, want the start failure")
	}
	if errors.Is(err, ErrNoOpener) {
		t.Fatalf("open error = %v, want a start failure rather than ErrNoOpener", err)
	}
	if !strings.Contains(err.Error(), "display refused the launch") {
		t.Fatalf("open error %q dropped the start failure", err)
	}
}

func TestCommandCandidatesByPlatform(t *testing.T) {
	cases := []struct {
		name string
		goos string
		want string
	}{
		{name: "mac", goos: "darwin", want: "open"},
		{name: "linux", goos: "linux", want: "xdg-open"},
		{name: "windows", goos: "windows", want: "rundll32.exe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commandCandidates(tc.goos, false, "https://example.com")
			if len(got) == 0 {
				t.Fatal("commandCandidates returned no commands")
			}
			if got[0].Name != tc.want {
				t.Fatalf("first opener = %q, want %q", got[0].Name, tc.want)
			}
		})
	}
}
