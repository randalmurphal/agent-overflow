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
	kindShowBrowser
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
)

// companionPlace is what a kind carries of the pane it came from.
type companionPlace int

const (
	// placeNone carries no pane mount: it keeps arrival order.
	placeNone companionPlace = iota
	// placeIfFromPane is a selection: from a pane it carries the pane's
	// mount, epoch and number, and with none it keeps arrival order.
	placeIfFromPane
	// placeNumbered comes only from a pane and carries its mount, epoch and
	// number: a close or a move, which reads or changes the strip's order
	// and takes its turn in the user's order.
	placeNumbered
	// placeMount comes only from a pane and carries its mount alone: a jar
	// switch, which acts on the lifetime that pane attached to (BindMount).
	placeMount
	// placeFocus carries the pane's mount and an Intent (page focus).
	placeFocus
)

// companionSpec is what a kind requires of its action.
type companionSpec struct {
	kind   companionKind
	target companionTarget
	place  companionPlace
	// download requires a DownloadID.
	download bool
}

var companionSpecs = map[string]companionSpec{
	"navigate":            {kind: kindNavigate, target: targetNamed},
	"back":                {kind: kindBack, target: targetNamed},
	"forward":             {kind: kindForward, target: targetNamed},
	"reload":              {kind: kindReload, target: targetNamed},
	"stop":                {kind: kindStop, target: targetNamed},
	"activate":            {kind: kindActivate, target: targetNamed, place: placeIfFromPane},
	"restore":             {kind: kindRestore, target: targetNamed},
	"step":                {kind: kindStep, place: placeIfFromPane},
	"move":                {kind: kindMove, target: targetNamed, place: placeNumbered},
	"show":                {kind: kindShow, target: targetOptional},
	"hide":                {kind: kindHide},
	"new":                 {kind: kindNew, place: placeIfFromPane},
	"close":               {kind: kindClose, target: targetNamed, place: placeNumbered},
	"close-others":        {kind: kindCloseOthers, target: targetNamed, place: placeNumbered},
	"close-right":         {kind: kindCloseRight, target: targetNamed, place: placeNumbered},
	"reopen":              {kind: kindReopen, place: placeIfFromPane},
	"duplicate":           {kind: kindDuplicate, target: targetNamed, place: placeIfFromPane},
	"devtools":            {kind: kindDevtools, target: targetNamed},
	"jar":                 {kind: kindJar, place: placeMount},
	"download-cancel":     {kind: kindDownloadCancel, download: true},
	"download-reveal":     {kind: kindDownloadReveal, download: true},
	"download-open":       {kind: kindDownloadOpen, download: true, place: placeIfFromPane},
	"downloads-clear":     {kind: kindDownloadsClear},
	"focus-page":          {kind: kindFocusPage, target: targetNamed, place: placeFocus},
	"focus-page-withdraw": {kind: kindFocusPageWithdraw, target: targetOptional, place: placeFocus},
	// The chat header's show: names no tab; the Manager shows the one
	// selected at its turn or opens one blank tab (ShowCompanion).
	"show-browser": {kind: kindShowBrowser, place: placeIfFromPane},
}

// companionCommand is a BrowserCompanionAction that parseCompanionAction
// accepted: its kind, and the fields that kind requires, present.
type companionCommand struct {
	kind     companionKind
	page     appbrowser.PageID
	address  string
	index    int
	jar      string
	download appbrowser.DownloadKey
	// place is the command's numbered place in its pane mount's order:
	// always set for placeNumbered, and for a selection that carries one.
	place    appbrowser.Numbered
	numbered bool
	// mount is the pane a jar switch was chosen in (placeMount); with
	// intent it names a page focus intent (placeFocus).
	mount  appbrowser.MountID
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
// command names its tab, a close or a move carries its pane's mount, epoch
// and number, a selection all three or none, and a jar switch its pane's
// mount.
func parseCompanionAction(action BrowserCompanionAction) (companionCommand, error) {
	name := strings.ToLower(strings.TrimSpace(action.Kind))
	spec, ok := companionSpecs[name]
	if !ok {
		return companionCommand{}, fmt.Errorf("browser: unsupported companion action %q", truncateKind(action.Kind))
	}
	page := strings.TrimSpace(action.PageID)
	mount := appbrowser.MountID(strings.TrimSpace(action.Mount))
	refuse := func(why string) (companionCommand, error) {
		return companionCommand{}, fmt.Errorf("%w: %s %s", errCompanionActionShape, name, why)
	}
	switch spec.target {
	case targetNamed:
		if page == "" {
			return refuse("names the tab it acts on")
		}
	case targetNone:
		if page != "" {
			return refuse("names no tab")
		}
	case targetOptional:
	default:
		return refuse("has no target rule")
	}
	cmd := companionCommand{
		kind: spec.kind, page: appbrowser.PageID(page), address: action.Address, index: action.Index,
		jar: strings.TrimSpace(action.Jar), download: appbrowser.DownloadKey(strings.TrimSpace(action.DownloadID)),
	}
	fromPane := mount != "" || action.Select != 0 || action.Epoch != 0
	switch spec.place {
	case placeFocus:
		if mount == "" || action.Intent == 0 || action.Select != 0 || action.Epoch != 0 {
			return refuse("needs its pane mount and intent")
		}
		cmd.mount, cmd.intent = mount, action.Intent
	case placeMount:
		if mount == "" || action.Select != 0 || action.Epoch != 0 || action.Intent != 0 {
			return refuse("needs the pane mount it was chosen in")
		}
		cmd.mount = mount
	case placeNumbered, placeIfFromPane:
		if action.Intent != 0 {
			return refuse("carries no focus intent")
		}
		if spec.place == placeIfFromPane && !fromPane {
			break
		}
		place, err := appbrowser.NewNumbered(mount, action.Epoch, action.Select)
		if err != nil {
			return companionCommand{}, fmt.Errorf("%w: %s: %w", errCompanionActionShape, name, err)
		}
		cmd.place, cmd.numbered = place, true
	case placeNone:
		if fromPane || action.Intent != 0 {
			return refuse("is not numbered in a pane")
		}
	default:
		return refuse("has no pane rule")
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

// browserJarPaneGoneCode is the code of a jar switch refused because the
// pane it was chosen in is no longer the thread's: it detached, or the
// thread's browser it showed was closed (BindMount).
const browserJarPaneGoneCode = "browser_jar_pane_gone"

// jarSwitchError is the answer to a jar switch: the Manager's refusal of an
// unconfirmed one, and of one whose pane is gone, as coded errors the pane
// recognizes, any other error as it is.
func jarSwitchError(err error) error {
	switch {
	case errors.Is(err, appbrowser.ErrJarSwitchUnconfirmed):
		return errorsx.Public(browserJarUnconfirmedCode, "Switching the cookie jar closes the open tabs and loses their page state", err)
	case errors.Is(err, appbrowser.ErrMountGone), errors.Is(err, appbrowser.ErrThreadClosed):
		return errorsx.Public(browserJarPaneGoneCode, "The cookie jar was not changed: the browser pane it was chosen in has closed", err)
	}
	return err
}

// browserDownloadUnavailableCode is the code of an action on a download the
// thread's list no longer has; the window shows it as a notice.
const browserDownloadUnavailableCode = "browser_download_unavailable"

// downloadActionError is the answer to an action on one download: the
// Manager's ErrDownloadUnavailable as a coded error, any other as it is.
func downloadActionError(err error) error {
	if errors.Is(err, appbrowser.ErrDownloadUnavailable) {
		return errorsx.Public(browserDownloadUnavailableCode, "That download is no longer in this browser's list", err)
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
