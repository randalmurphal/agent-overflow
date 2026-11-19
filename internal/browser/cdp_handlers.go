package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/fetch"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// installHandlers subscribes to the page's events before enabling the
// domains that send them, so none is lost.
func (p *cdpPage) installHandlers() error {
	ctx, logf := p.ctx, log.Printf
	session := chromedp.FromContext(ctx).Target
	onTargetEvent(ctx, page.JavascriptDialogOpening, logf, func(event page.EventJavascriptDialogOpening) {
		accept := event.Type == page.DialogTypeBeforeunload
		answerCtx, cancel := operationContext(context.Background(), ctx, 3*time.Second)
		defer cancel()
		if _, err := cdp.Call(answerCtx, session, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: accept}); err != nil && ctx.Err() == nil {
			logf("browser: answer a %s dialog: %v", event.Type, err)
		}
	})
	onTargetEvent(ctx, page.FrameAttached, logf, func(event page.EventFrameAttached) {
		p.frames.attached(event.FrameID)
	})
	// Page.frameNavigated fires when a frame is associated with a new loader,
	// that is a new document; a same-document navigation fires
	// Page.navigatedWithinDocument instead (DevTools protocol, Page domain).
	// The commit is ordered where the event arrives, on chromedp's event
	// goroutine; nothing on that goroutine waits for the Manager.
	onTargetEvent(ctx, page.FrameNavigated, logf, func(event page.EventFrameNavigated) {
		if event.Frame == nil {
			return
		}
		p.frames.navigated(event.Frame.ID)
		if event.Frame.ParentID == "" && p.hooks.DocumentCommitted != nil {
			if finish := p.hooks.DocumentCommitted(); finish != nil {
				finish()
			}
		}
	})
	onTargetEvent(ctx, page.FrameDetached, logf, func(event page.EventFrameDetached) {
		p.frames.detached(event.FrameID)
	})
	// A frame that attached before the subscriptions above, as a popup's
	// iframe made before its adoption did, sends no event of its own: the
	// frame tree names it, read once they are in place.
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return fmt.Errorf("browser: read the page's frames: %w", err)
	}
	p.frames.seed(tree.FrameTree)
	// The main frame's id is the target's.
	onTargetEvent(ctx, page.FrameStartedLoading, logf, func(event page.EventFrameStartedLoading) {
		if string(event.FrameID) == p.handle {
			p.reportLoading(p.loads.started(time.Now()))
		}
	})
	onTargetEvent(ctx, page.FrameStoppedLoading, logf, func(event page.EventFrameStoppedLoading) {
		if string(event.FrameID) == p.handle {
			p.reportLoading(p.loads.stopped(time.Now()))
		}
	})
	// chromedp.Console is the one ordered join of several event methods that
	// chromedp offers: console API calls and browser log entries reach the
	// log in the order the page wrote them.
	messages := chromedp.Console(ctx)
	go func() {
		for message, err := range messages {
			if err != nil {
				if ctx.Err() == nil {
					logf("browser: console events stopped: %v", err)
				}
				return
			}
			if entry, ok := consoleEntry(message, p.hooks.PageURL); ok {
				p.hooks.Console(entry)
			}
		}
	}()
	onTargetEvent(ctx, network.RequestWillBeSent, logf, func(event network.EventRequestWillBeSent) {
		p.network.started(event.RequestID, event.RedirectResponse != nil, time.Now())
	})
	onTargetEvent(ctx, network.LoadingFinished, logf, func(event network.EventLoadingFinished) {
		p.network.ended(event.RequestID, time.Now())
	})
	onTargetEvent(ctx, network.LoadingFailed, logf, func(event network.EventLoadingFailed) {
		p.network.ended(event.RequestID, time.Now())
	})
	if err := p.watchDocumentTitle(); err != nil {
		return err
	}
	if _, err := chromedp.Call(ctx, cdplog.Enable, cdp.Empty{}); err != nil {
		return fmt.Errorf("browser: enable console log capture: %w", err)
	}
	if _, err := chromedp.Call(ctx, cdpruntime.Enable, cdp.Empty{}); err != nil {
		return fmt.Errorf("browser: enable runtime capture: %w", err)
	}
	if _, err := chromedp.Call(ctx, network.Enable, networkEnableParams()); err != nil {
		return fmt.Errorf("browser: enable network lifecycle: %w", err)
	}
	// chromedp's attach enables CSS, which sends an event for every style
	// sheet a document adds and every media query result that changes.
	// Nothing reads them (chromedp only needs CSS for ComputedStyle and
	// MatchedStyle, which no driver uses), and on the hosted engine each one
	// crosses to the backend through the launcher's relay.
	if _, err := chromedp.Call(ctx, css.Disable, cdp.Empty{}); err != nil {
		return fmt.Errorf("browser: disable unread style sheet events: %w", err)
	}
	return nil
}

// networkEnableParams enables the Network domain for its request lifecycle
// events alone. By default Chromium keeps every page's response bodies and
// request post data for Network.getResponseBody (200 MB per page, 20 MB per
// resource) and copies post data of any size into each requestWillBeSent
// event. Nothing here reads either: browser_assets streams its bodies
// through Network.loadNetworkResource, which does not use that buffer. So the
// buffers are zero, and post data is capped at one byte because Chromium
// reads a zero post-data cap as unlimited. chromedp's attach has already
// enabled Network with the defaults; this later enable replaces the limits.
func networkEnableParams() network.EnableParams {
	return network.EnableParams{
		MaxTotalBufferSize:    new(int64(0)),
		MaxResourceBufferSize: new(int64(0)),
		MaxPostDataSize:       new(int64(1)),
	}
}

// navigationPolicyPatterns are the requests the navigation policy decides.
// Document-only interception would still let a workspace HTML page embed an
// outside-workspace file as an image or script, so every local-file request
// is intercepted too.
var navigationPolicyPatterns = []*fetch.RequestPattern{
	{ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest},
	{URLPattern: "file://*", RequestStage: fetch.RequestStageRequest},
}

// answerPausedRequest continues a request the navigation policy paused when
// allow permits its URL and fails it otherwise. session is the one that
// paused it. The answer runs on its own goroutine, bounded by lifetime, so a
// slow check or round trip does not hold the requests paused behind it.
func answerPausedRequest(lifetime context.Context, session cdp.Session, event fetch.EventRequestPaused, allow func(url string) bool) {
	if event.Request == nil {
		return
	}
	requestID, rawURL := event.RequestID, event.Request.URL
	go func() {
		// Decided before the answer's own bound starts: the decision may
		// wait for the request's owner (awaitRequestOwner).
		allowed := allow(rawURL)
		ctx, cancel := operationContext(context.Background(), lifetime, 5*time.Second)
		defer cancel()
		var err error
		if allowed {
			_, err = cdp.Call(ctx, session, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: requestID})
		} else {
			_, err = cdp.Call(ctx, session, fetch.FailRequest, fetch.FailRequestParams{RequestID: requestID, ErrorReason: network.ErrorReasonBlockedByClient})
		}
		// A page or browser that has gone needs no answer. Any other refusal
		// leaves the request paused, which the page sees as a load that never
		// finishes, so it is logged.
		if err != nil && lifetime.Err() == nil {
			log.Printf("browser: answer paused request %s: %v", requestID, err)
		}
	}()
}

// requestOwnerWait bounds how long a paused request that needs an owner's
// authority, and whose owners no page answers to yet, waits for one: its
// target's creation, which chromedp delivers on a subscription of its own,
// in no fixed order with the pause; its popup's report from the launcher;
// or its frame's attach. Past it the request is refused. requestOwnerPoll
// is how often it asks again.
const (
	requestOwnerWait = 2 * time.Second
	requestOwnerPoll = 5 * time.Millisecond
)

// awaitRequestOwner answers a paused request with decide's answer once
// decide resolves an owner, or at once when decide allows it with none (a
// page with no workspace's authority may make it). Otherwise it asks again
// every requestOwnerPoll for up to wait (requestOwnerWait when zero), and
// refuses the request when no owner resolves by then or lifetime ends.
// Runs off the event reader (answerPausedRequest).
func awaitRequestOwner(lifetime context.Context, wait time.Duration, decide func() (allowed, resolved bool), logf func(string, ...any)) bool {
	if wait <= 0 {
		wait = requestOwnerWait
	}
	deadline := time.Now().Add(wait)
	for {
		allowed, resolved := decide()
		if resolved || allowed {
			return allowed
		}
		if !time.Now().Before(deadline) {
			logf("browser: refuse a request no page answered for within %v", wait)
			return false
		}
		select {
		case <-lifetime.Done():
			return false
		case <-time.After(requestOwnerPoll):
		}
	}
}

// consoleEntry converts one message of chromedp.Console. Uncaught exceptions
// were never part of the console log, so they are skipped. A console API
// call carries no URL of its own, so the page's last known one is used.
func consoleEntry(message chromedp.ConsoleMessage, pageURL func() string) (ConsoleLog, bool) {
	if message.IsException() {
		return ConsoleLog{}, false
	}
	timestamp := message.Time.UTC()
	if message.Time.IsZero() || message.Time.Unix() == 0 {
		timestamp = time.Now().UTC()
	}
	if message.Source != "" {
		// A browser log entry: only those carry a source. Console reports
		// the log level verbose as debug, which the log has always shown as
		// log.
		level := string(message.Type)
		if message.Type == chromedp.ConsoleDebug {
			level = "log"
		}
		return ConsoleLog{
			Level: normalizeConsoleLevel(level), Message: message.Text,
			Timestamp: timestamp.Format(time.RFC3339Nano), URL: message.URL,
		}, true
	}
	parts := make([]string, 0, len(message.Args))
	for _, arg := range message.Args {
		if arg == nil {
			continue
		}
		var value any
		if len(arg.Value) > 0 && json.Unmarshal(arg.Value, &value) == nil {
			parts = append(parts, fmt.Sprint(value))
		} else if arg.Description != "" {
			parts = append(parts, arg.Description)
		} else {
			parts = append(parts, string(arg.Type))
		}
	}
	return ConsoleLog{
		Level: normalizeConsoleLevel(string(message.Type)), Message: strings.Join(parts, " "),
		Timestamp: timestamp.Format(time.RFC3339Nano), URL: pageURL(),
	}, true
}
