package app

import (
	"errors"
	"testing"

	"agent-overflow/internal/externalurl"
)

// An isolated boot opens no system browser: a link or a sign-in page answers
// as a host with no opener does, which a sign-in turns into its remote link.
// A malformed URL is still refused for what it is.
func TestAnIsolatedBootOpensNoSystemBrowser(t *testing.T) {
	t.Parallel()
	app := &App{}
	ConfigureIsolation(app, IsolationConfig{})

	if err := app.OpenExternalURL("https://example.com/"); !errors.Is(err, externalurl.ErrNoOpener) {
		t.Fatalf("OpenExternalURL on an isolated boot = %v, want ErrNoOpener", err)
	}
	if err := app.OpenExternalURL("file:///etc/passwd"); err == nil || errors.Is(err, externalurl.ErrNoOpener) {
		t.Fatalf("OpenExternalURL(file URL) = %v, want the validation refusal", err)
	}
}
