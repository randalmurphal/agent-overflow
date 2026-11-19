package app

import (
	"errors"
	"fmt"
	"strings"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/errorsx"
)

// companionKind is a BrowserCompanionAction's kind, parsed once at the
// decoder (parseCompanionAction); nothing after it reads the wire's string.
type companionKind int

const (
	kindNavigate companionKind = iota + 1
	kindBack
	kindForward
	kindReload
	kindStop
	kindActivate
	kindRestore
	kindStep
	kindMove
	kindShow
	kindHide
	kindNew
	kindClose
	kindCloseOthers
	kindCloseRight
	kindReopen
	kindDuplicate
	kindDevtools
	kindJar
	kindDownloadCancel
	kindDownloadReveal
	kindDownloadOpen
	kindDownloadsClear
	kindFocusPage
	kindFocusPageWithdraw
	kindCloseActive
	kindReloadActive
)

// companionTarget is which tab a kind acts on.
type companionTarget int

const (
	// targetNone acts on no tab: the action names none.
	targetNone companionTarget = iota
	// targetNamed acts on the tab it names, which it must.
	targetNamed
	// targetOptional may name a tab (show).
	targetOptional
	// targetActive acts on the tab active at its numbered turn: it names
	// none and must carry its pane mount and number.
	targetActive
)

// companionSpec is what a kind requires of its action.
type companionSpec struct {
	kind   companionKind
	target companionTarget
	// selects is a selection, which may carry its pane mount and number
	// (both or neither) and otherwise keeps arrival order.
	selects bool
	// download requires a DownloadID; focus a Mount and an Intent.
	download, focus bool
}

var companionSpecs = map[string]companionSpec{
	"navigate":            {kind: kindNavigate, target: targetNamed},
	"back":                {kind: kindBack, target: targetNamed},
	"forward":             {kind: kindForward, target: targetNamed},
	"reload":              {kind: kindReload, target: targetNamed},
	"stop":                {kind: kindStop, target: targetNamed},
	"activate":            {kind: kindActivate, target: targetNamed, selects: true},
	"restore":             {kind: kindRestore, target: targetNamed},
	"step":                {kind: kindStep, selects: true},
	"move":                {kind: kindMove, target: targetNamed},
	"show":                {kind: kindShow, target: targetOptional},
	"hide":                {kind: kindHide},
	"new":                 {kind: kindNew, selects: true},
	"close":               {kind: kindClose, target: targetNamed},
	"close-others":        {kind: kindCloseOthers, target: targetNamed},
	"close-right":         {kind: kindCloseRight, target: targetNamed},
	"reopen":              {kind: kindReopen, selects: true},
	"duplicate":           {kind: kindDuplicate, target: targetNamed, selects: true},
	"devtools":            {kind: kindDevtools, target: targetNamed},
	"jar":                 {kind: kindJar},
	"download-cancel":     {kind: kindDownloadCancel, download: true},
	"download-reveal":     {kind: kindDownloadReveal, download: true},
	"download-open":       {kind: kindDownloadOpen, download: true, selects: true},
	"downloads-clear":     {kind: kindDownloadsClear},
	"focus-page":          {kind: kindFocusPage, target: targetNamed, focus: true},
	"focus-page-withdraw": {kind: kindFocusPageWithdraw, target: targetOptional, focus: true},
	"close-active":        {kind: kindCloseActive, target: targetActive},
	"reload-active":       {kind: kindReloadActive, target: targetActive},
}

// companionCommand is a BrowserCompanionAction that parseCompanionAction
// accepted: its kind, and the fields that kind requires, present.
type companionCommand struct {
	kind     companionKind
	page     string
	address  string
	index    int
	jar      string
	download string
	// place is the command's numbered place in its pane mount's order:
	// always set for targetActive, and for a selection that carries one.
	place    appbrowser.Numbered
	numbered bool
	// mount and intent name a page focus intent (focus kinds).
	mount  string
	intent uint64
	// consent is a jar switch's: confirmed when the user agreed that it
	// closes the live tabs, otherwise refused while a tab is live. Set for
	// kindJar only.
	consent appbrowser.JarConsent
}

var errCompanionActionShape = errors.New("browser: malformed companion action")

// errNoCompanionHandler is a parsed kind BrowserCompanionDo does not handle.
var errNoCompanionHandler = errors.New("browser: companion action has no handler")

// parseCompanionAction validates action against its kind's spec: a named
// command names its tab and an active-tab command names none and carries
// its mount, epoch and number; a selection carries all three or none.
func parseCompanionAction(action BrowserCompanionAction) (companionCommand, error) {
	name := strings.ToLower(strings.TrimSpace(action.Kind))
	spec, ok := companionSpecs[name]
	if !ok {
		return companionCommand{}, fmt.Errorf("browser: unsupported companion action %q", truncateKind(action.Kind))
	}
	page := strings.TrimSpace(action.PageID)
	mount := strings.TrimSpace(action.Mount)
	refuse := func(why string) (companionCommand, error) {
		return companionCommand{}, fmt.Errorf("%w: %s %s", errCompanionActionShape, name, why)
	}
	switch spec.target {
	case targetNamed:
		if page == "" {
			return refuse("names the tab it acts on")
		}
	case targetActive, targetNone:
		if page != "" {
			return refuse("names no tab")
		}
	case targetOptional:
	default:
		return refuse("has no target rule")
	}
	cmd := companionCommand{
		kind: spec.kind, page: page, address: action.Address, index: action.Index,
		jar: strings.TrimSpace(action.Jar), download: strings.TrimSpace(action.DownloadID),
	}
	switch {
	case spec.focus:
		if mount == "" || action.Intent == 0 || action.Select != 0 || action.Epoch != 0 {
			return refuse("needs its pane mount and intent")
		}
		cmd.mount, cmd.intent = mount, action.Intent
	case spec.target == targetActive || (spec.selects && (mount != "" || action.Select != 0 || action.Epoch != 0)):
		place, err := appbrowser.NewNumbered(appbrowser.MountID(mount), action.Epoch, action.Select)
		if err != nil {
			return companionCommand{}, fmt.Errorf("%w: %s: %w", errCompanionActionShape, name, err)
		}
		cmd.place, cmd.numbered = place, true
	case mount != "" || action.Select != 0 || action.Epoch != 0 || action.Intent != 0:
		return refuse("is not numbered in a pane")
	}
	if spec.download && cmd.download == "" {
		return refuse("names its download")
	}
	if spec.kind == kindJar && cmd.jar == "" {
		return refuse("names its cookie jar")
	}
	switch {
	case spec.kind == kindJar && action.Confirmed:
		cmd.consent = appbrowser.JarSwitchConfirmed
	case spec.kind == kindJar:
		cmd.consent = appbrowser.JarSwitchIfNoLiveTab
	case action.Confirmed:
		return refuse("asks no confirmation")
	}
	return cmd, nil
}

// browserJarUnconfirmedCode is the code of an unconfirmed jar switch the
// Manager refused because a tab is live; the pane asks the user on it.
const browserJarUnconfirmedCode = "browser_jar_unconfirmed"

// jarSwitchError is the answer to a jar switch: the Manager's refusal of an
// unconfirmed one as a coded error the pane recognizes, any other error as
// it is.
func jarSwitchError(err error) error {
	if errors.Is(err, appbrowser.ErrJarSwitchUnconfirmed) {
		return errorsx.Public(browserJarUnconfirmedCode, "Switching the cookie jar closes the open tabs and loses their page state", err)
	}
	return err
}

// truncateKind bounds an unknown kind quoted back in an error.
func truncateKind(kind string) string {
	if runes := []rune(kind); len(runes) > 32 {
		return string(runes[:32])
	}
	return kind
}

// errActiveOutOfOrder refuses a keyboard command on the active tab whose
// turn came before an earlier command of its pane had arrived.
var errActiveOutOfOrder = errors.New("browser: an earlier tab action from this pane had not arrived, so the active tab was not known; nothing was changed. Try again")

// activeOutcomeError is the answer to a keyboard command on the active tab:
// a refusal for ActiveOutOfOrder, nothing for every outcome that is the
// state the user asked for or a quiet no-op.
func activeOutcomeError(outcome appbrowser.ActiveOutcome) error {
	switch outcome {
	case appbrowser.ActiveApplied, appbrowser.ActiveNoTab, appbrowser.ActivePassed:
		return nil
	case appbrowser.ActiveOutOfOrder:
		return errActiveOutOfOrder
	default:
		return fmt.Errorf("browser: unknown outcome %d of a tab action", outcome)
	}
}
