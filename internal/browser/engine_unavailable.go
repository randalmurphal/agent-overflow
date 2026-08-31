package browser

import (
	"context"
	"errors"
)

// unavailableEngine is what a deployment WITHOUT a browser engine has instead
// of one (spec §9: the engines live in the desktop app instance, so a remote
// `--connect` backend, a headless serve mode and `go test` get no pane and no
// browser tools).
//
// It is a value rather than a nil `browserEngine` on purpose: every Manager
// path — start, teardown, the pane presentation sync, devtools — would
// otherwise need its own nil check, and one missed check is a nil deref inside
// a tool call. Here the refusal is a single sentence in a single place, and
// the capability assertions (`paneHost`, `paneDevTools`) simply fail on it.
type unavailableEngine struct{}

// errNoBrowserEngine is the one user-facing sentence. It reaches the model as
// the browser tool's error and the user as the companion pane's.
var errNoBrowserEngine = errors.New("browser: browser tools are not available in this deployment")

func (unavailableEngine) Start(context.Context) error { return errNoBrowserEngine }
func (unavailableEngine) Running() bool               { return false }
func (unavailableEngine) Interrupt()                  {}
func (unavailableEngine) Stop()                       {}
func (unavailableEngine) DiscardPage(string)          {}

func (unavailableEngine) NewProfile(context.Context, profileOptions) (engineProfile, error) {
	return nil, errNoBrowserEngine
}
