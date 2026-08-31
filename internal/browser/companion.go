package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

const (
	companionFrameInterval  = time.Second / 15
	companionJPEGQuality    = 65
	minCompanionWidth       = 320
	minCompanionHeight      = 240
	maxCompanionWidth       = 1920
	maxCompanionHeight      = 1200
	maxCompanionSubscribers = 64
	maxCompanionText        = 1 << 20
	maxCompanionKey         = 256
)

type companionSubscriber struct {
	threadID string
	width    int
	height   int
	frames   chan CompanionEvent
	done     chan struct{}
}

type pageStream struct {
	width  int
	height int
	done   chan struct{}
	frames chan CompanionEvent
	seq    uint64
	ready  bool
}

func clampViewport(width, height int) (int, int) {
	if width < minCompanionWidth {
		width = minCompanionWidth
	}
	if height < minCompanionHeight {
		height = minCompanionHeight
	}
	if width > maxCompanionWidth {
		width = maxCompanionWidth
	}
	if height > maxCompanionHeight {
		height = maxCompanionHeight
	}
	return width, height
}

func (p *managedPage) setInfo(info PageInfo) {
	p.metaMu.Lock()
	info.Label = p.info.Label
	p.info = info
	p.metaMu.Unlock()
}

func (p *managedPage) setLabel(label string) PageInfo {
	p.metaMu.Lock()
	p.info.Label = label
	info := p.info
	p.metaMu.Unlock()
	return info
}

func (p *managedPage) cachedInfo() PageInfo {
	p.metaMu.RLock()
	info := p.info
	p.metaMu.RUnlock()
	return info
}

func (m *Manager) pageChanged(p *managedPage) {
	p.touch()
	m.ensureActivePage(p.owner, p.id)
	m.emitThreadState(p.owner)
	m.syncThreadStream(p.owner)
}

func (m *Manager) threadState(threadID string) CompanionEvent {
	m.mu.Lock()
	pages := make([]*managedPage, 0)
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				pages = append(pages, p)
			}
		}
	}
	session, hasSession := m.sessions[threadID]
	m.mu.Unlock()
	sort.Slice(pages, func(i, j int) bool { return pages[i].createdAt < pages[j].createdAt })
	event := CompanionEvent{Kind: "state", ThreadID: threadID, Pages: make([]PageInfo, 0, len(pages))}
	visible := false
	if hasSession {
		visible = session.Visible
		event.SessionName = session.Name
	}
	event.Visible = &visible
	for _, p := range pages {
		event.Pages = append(event.Pages, p.cachedInfo())
	}
	event.ActivePageID = session.ActivePageID
	return event
}

func (m *Manager) emit(event CompanionEvent) {
	m.mu.Lock()
	sink := m.eventSink
	m.mu.Unlock()
	if sink != nil {
		sink(event)
	}
}

func (m *Manager) emitThreadState(threadID string) {
	if strings.TrimSpace(threadID) != "" {
		m.emit(m.threadState(threadID))
	}
}

func (m *Manager) updatePageInfo(handle, url, title string) {
	m.mu.Lock()
	var found *managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.driver.Handle() == handle {
				found = p
				break
			}
		}
		if found != nil {
			break
		}
	}
	m.mu.Unlock()
	if found == nil {
		return
	}
	found.setInfo(PageInfo{ID: found.id, URL: truncateUTF8(url, maxBrowserURLBytes), Title: truncateUTF8(title, maxBrowserTitleBytes)})
	m.emitThreadState(found.owner)
}

func (m *Manager) SubscribeCompanion(access Access, width, height int) (CompanionSubscription, error) {
	state := m.threadState(access.ThreadID)
	if len(state.Pages) == 0 {
		return CompanionSubscription{}, fmt.Errorf("browser: thread has no open pages")
	}
	width, height = clampViewport(width, height)
	id := uuid.NewString()
	m.mu.Lock()
	if len(m.subscriptions) >= maxCompanionSubscribers {
		m.mu.Unlock()
		return CompanionSubscription{}, fmt.Errorf("browser: too many companion subscriptions")
	}
	m.subscriptions[id] = companionSubscriber{
		threadID: access.ThreadID,
		width:    width,
		height:   height,
		frames:   make(chan CompanionEvent, 1),
		done:     make(chan struct{}),
	}
	m.mu.Unlock()
	m.syncThreadStream(access.ThreadID)
	return CompanionSubscription{ID: id, State: state}, nil
}

func (m *Manager) CompanionState(access Access) CompanionEvent {
	return m.threadState(access.ThreadID)
}

func (m *Manager) UnsubscribeCompanion(id string) {
	m.mu.Lock()
	sub, ok := m.subscriptions[id]
	if ok {
		delete(m.subscriptions, id)
		close(sub.done)
	}
	m.mu.Unlock()
	if ok {
		m.syncThreadStream(sub.threadID)
	}
}

// NextCompanionFrame waits for the newest frame addressed to one companion
// subscription. Frames do not ride the global event bus: only the connection
// that mounted the pane pays the JPEG wire/JSON cost, and the capacity-one
// queue naturally backpressures a slow or backgrounded renderer.
func (m *Manager) NextCompanionFrame(ctx context.Context, id string) (CompanionEvent, error) {
	m.mu.Lock()
	sub, ok := m.subscriptions[strings.TrimSpace(id)]
	m.mu.Unlock()
	if !ok {
		return CompanionEvent{}, fmt.Errorf("browser: companion subscription not found")
	}
	select {
	case event := <-sub.frames:
		return event, nil
	case <-sub.done:
		return CompanionEvent{}, fmt.Errorf("browser: companion subscription closed")
	case <-ctx.Done():
		return CompanionEvent{}, ctx.Err()
	}
}

func (m *Manager) deliverCompanionFrame(event CompanionEvent) {
	m.mu.Lock()
	for _, sub := range m.subscriptions {
		if sub.threadID != event.ThreadID {
			continue
		}
		select {
		case sub.frames <- event:
		default:
			select {
			case <-sub.frames:
			default:
			}
			select {
			case sub.frames <- event:
			default:
			}
		}
	}
	m.mu.Unlock()
}

func (m *Manager) ResizeCompanion(id string, width, height int) error {
	width, height = clampViewport(width, height)
	m.mu.Lock()
	sub, ok := m.subscriptions[id]
	if ok {
		sub.width, sub.height = width, height
		m.subscriptions[id] = sub
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: companion subscription not found")
	}
	m.syncThreadStream(sub.threadID)
	return nil
}

func (m *Manager) syncThreadStream(threadID string) {
	m.mu.Lock()
	var pages []*managedPage
	var active *managedPage
	width, height := 0, 0
	hasSubscriber := false
	session, hasSession := m.sessions[threadID]
	visible := hasSession && session.Visible
	for _, sub := range m.subscriptions {
		if sub.threadID != threadID {
			continue
		}
		hasSubscriber = true
		if sub.width*sub.height > width*height {
			width, height = sub.width, sub.height
		}
	}
	if hasSubscriber && session.ViewportSet {
		width, height = session.ViewportW, session.ViewportH
	}
	if !visible {
		width, height = 0, 0
	}
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner != threadID {
				continue
			}
			pages = append(pages, p)
			if p.id == session.ActivePageID {
				active = p
			}
		}
	}
	m.mu.Unlock()
	for _, p := range pages {
		if width == 0 || p != active {
			p.stopStream()
		}
	}
	if width > 0 && active != nil {
		active.startStream(m, width, height)
	}
	m.syncPanePresentation(pages, active, visible)
}

// syncPanePresentation drives an engine whose pages are real windows.
//
// This is the same decision the screencast selection above makes — WHICH
// of a thread's pages is presented, and whether the pane is showing at all
// — expressed to an engine that answers it by moving a window instead of
// by starting a frame stream. The decision itself stays here, in the
// Manager: the engine is told the outcome, never the rule.
//
// Managed Chrome has no window and no paneHost, so this is a type
// assertion that fails and costs nothing on every other deployment.
//
// Where the pane SITS is not decided here. Bounds come from the frontend's
// host rect, which reaches paneHost.SetPageBounds through its own binding;
// a controller with no bounds yet is shown at whatever the launcher last
// positioned it to.
func (m *Manager) syncPanePresentation(pages []*managedPage, active *managedPage, visible bool) {
	host, ok := m.engine.(paneHost)
	if !ok {
		return
	}
	for _, p := range pages {
		if p == active && visible {
			continue
		}
		host.HidePage(p.driver.Handle())
	}
	if visible && active != nil {
		host.ShowPage(active.driver.Handle())
	}
}

func (p *managedPage) startStream(m *Manager, width, height int) {
	// The streamed pane is CDP screencast all the way down — it addresses the
	// page's chromedp context directly rather than going through the driver
	// seam — so it can only run on the managed-Chrome engine. Saying so is the
	// difference between an explained absence and a cryptic protocol error;
	// spec §7 replaces this whole surface with the presented native view, and
	// §9 deletes the screencast with it.
	if _, ok := p.driver.(*cdpPage); !ok {
		m.emit(CompanionEvent{
			Kind: "error", ThreadID: p.owner, PageID: p.id,
			Error: "browser: the companion pane is not available on this browser engine yet",
		})
		return
	}
	width, height = clampViewport(width, height)
	p.streamMu.Lock()
	if p.stream != nil && p.stream.width == width && p.stream.height == height {
		p.streamMu.Unlock()
		return
	}
	old := p.stream
	stream := &pageStream{
		width: width, height: height,
		done:   make(chan struct{}),
		frames: make(chan CompanionEvent, 1),
	}
	p.stream = stream
	p.streamMu.Unlock()
	if old != nil {
		close(old.done)
	}
	go stream.run(m)
	go func() {
		p.streamCmdMu.Lock()
		defer p.streamCmdMu.Unlock()
		p.streamMu.Lock()
		current := p.stream == stream
		p.streamMu.Unlock()
		if !current {
			return
		}
		ctx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
		defer cancel()
		targetCtx := targetCommandContext(ctx)
		_ = page.StopScreencast().Do(targetCtx)
		if err := p.driver.SetViewport(ctx, width, height); err != nil {
			m.emit(CompanionEvent{Kind: "error", ThreadID: p.owner, PageID: p.id, Error: err.Error()})
			return
		}
		// Chrome may deliver the first (and, for a static page, only) frame
		// before StartScreencast returns. Arm the stream before issuing the
		// command so that frame cannot be discarded by the target callback.
		p.streamMu.Lock()
		if p.stream == stream {
			stream.ready = true
		}
		p.streamMu.Unlock()
		if err := page.StartScreencast().WithFormat(page.ScreencastFormatJpeg).WithQuality(companionJPEGQuality).WithMaxWidth(int64(width)).WithMaxHeight(int64(height)).WithEveryNthFrame(2).Do(targetCtx); err != nil {
			p.streamMu.Lock()
			if p.stream == stream {
				stream.ready = false
			}
			p.streamMu.Unlock()
			m.emit(CompanionEvent{Kind: "error", ThreadID: p.owner, PageID: p.id, Error: err.Error()})
			return
		}
		// Screencast is damage-driven: a page that was already fully painted
		// can produce no event at all. Seed a single frame after startup, but
		// skip it when the stream callback already delivered one.
		if err := p.seedInitialStreamFrame(stream); err != nil {
			m.emit(CompanionEvent{Kind: "error", ThreadID: p.owner, PageID: p.id, Error: err.Error()})
		}
	}()
}

func (p *managedPage) seedInitialStreamFrame(stream *pageStream) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		p.streamMu.Lock()
		current := p.stream == stream
		alreadyDelivered := stream.seq > 0
		p.streamMu.Unlock()
		if !current || alreadyDelivered {
			return nil
		}
		ctx, cancel := operationContext(context.Background(), p.ctx, 3*time.Second)
		initial, err := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatJpeg).
			WithQuality(companionJPEGQuality).
			WithFromSurface(true).
			WithOptimizeForSpeed(true).
			Do(targetCommandContext(ctx))
		cancel()
		if err == nil && len(initial) == 0 {
			err = fmt.Errorf("empty JPEG")
		}
		if err == nil {
			p.streamMu.Lock()
			if p.stream == stream && stream.seq == 0 {
				stream.seq++
				frame := CompanionEvent{
					Kind: "frame", ThreadID: p.owner, PageID: p.id,
					Frame: base64.StdEncoding.EncodeToString(initial),
					Width: stream.width, Height: stream.height, Sequence: stream.seq,
				}
				select {
				case stream.frames <- frame:
				default:
				}
			}
			p.streamMu.Unlock()
			return nil
		}
		lastErr = err
		select {
		case <-stream.done:
			return nil
		case <-p.ctx.Done():
			return nil
		case <-time.After(75 * time.Millisecond):
		}
	}
	return fmt.Errorf("browser: seed companion frame after retries: %w", lastErr)
}

func (p *managedPage) stopStream() {
	p.streamMu.Lock()
	stream := p.stream
	p.stream = nil
	p.streamMu.Unlock()
	if stream == nil {
		return
	}
	close(stream.done)
	go func() {
		p.streamCmdMu.Lock()
		defer p.streamCmdMu.Unlock()
		p.streamMu.Lock()
		replaced := p.stream != nil
		p.streamMu.Unlock()
		if replaced {
			return
		}
		ctx, cancel := operationContext(context.Background(), p.ctx, 3*time.Second)
		defer cancel()
		_ = page.StopScreencast().Do(targetCommandContext(ctx))
	}()
}

func (s *pageStream) run(m *Manager) {
	ticker := time.NewTicker(companionFrameInterval)
	defer ticker.Stop()
	var pending *CompanionEvent
	for {
		select {
		case <-s.done:
			return
		case frame := <-s.frames:
			pending = &frame
		case <-ticker.C:
			if pending != nil {
				m.deliverCompanionFrame(*pending)
				pending = nil
			}
		}
	}
}

func (m *Manager) handleScreencastFrame(p *managedPage, data string) {
	p.streamMu.Lock()
	stream := p.stream
	if stream == nil || !stream.ready {
		p.streamMu.Unlock()
		return
	}
	stream.seq++
	frame := CompanionEvent{Kind: "frame", ThreadID: p.owner, PageID: p.id, Frame: data, Width: stream.width, Height: stream.height, Sequence: stream.seq}
	select {
	case stream.frames <- frame:
	default:
		select {
		case <-stream.frames:
		default:
		}
		select {
		case stream.frames <- frame:
		default:
		}
	}
	p.streamMu.Unlock()
}

func (m *Manager) NewCompanionPage(ctx context.Context, access Access) (PageInfo, error) {
	p, err := m.createPage(ctx, access)
	if err != nil {
		return PageInfo{}, err
	}
	if _, err := m.SelectPage(ctx, access, p.id); err != nil {
		return PageInfo{}, err
	}
	return p.cachedInfo(), nil
}

func (m *Manager) ActivateCompanionPage(access Access, pageID string) error {
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	p.touch()
	m.setActivePage(access.ThreadID, p.id)
	m.emitThreadState(access.ThreadID)
	m.syncThreadStream(access.ThreadID)
	return nil
}

func (m *Manager) NavigateCompanion(ctx context.Context, access Access, pageID, address string) (PageInfo, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return PageInfo{}, fmt.Errorf("browser: address is required")
	}
	lower := strings.ToLower(address)
	parsed, _ := url.Parse(address)
	if parsed != nil && strings.EqualFold(parsed.Scheme, "file") {
		return m.OpenFile(ctx, access, filepath.FromSlash(parsed.Path), OpenOptions{PageID: pageID})
	}
	if filepath.IsAbs(address) {
		return m.OpenFile(ctx, access, address, OpenOptions{PageID: pageID})
	}
	workspaceFile := filepath.Join(access.Workspace, filepath.FromSlash(address))
	if info, err := os.Stat(workspaceFile); err == nil && info.Mode().IsRegular() {
		return m.OpenFile(ctx, access, workspaceFile, OpenOptions{PageID: pageID})
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		host := address
		if before, _, ok := strings.Cut(address, ":"); ok {
			host = before
		}
		isHost := strings.Contains(host, ".") || strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil
		if strings.ContainsAny(address, " \t\r\n") || !isHost {
			address = "https://www.google.com/search?q=" + url.QueryEscape(address)
		} else if strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil {
			address = "http://" + address
		} else {
			address = "https://" + address
		}
	}
	return m.Open(ctx, access, address, OpenOptions{PageID: pageID})
}

func (m *Manager) CompanionInput(ctx context.Context, access Access, pageID string, event CompanionInput) error {
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	defer cancel()
	targetCtx := targetCommandContext(opCtx)
	switch strings.ToLower(strings.TrimSpace(event.Kind)) {
	case "text":
		if event.Text == "" {
			return nil
		}
		if len(event.Text) > maxCompanionText {
			return fmt.Errorf("browser: companion text exceeds %d bytes", maxCompanionText)
		}
		err = input.InsertText(event.Text).Do(targetCtx)
	case "key":
		if len(event.Key) == 0 || len(event.Key) > maxCompanionKey {
			return fmt.Errorf("browser: companion key must be between 1 and %d bytes", maxCompanionKey)
		}
		key := event.Key
		if event.Control {
			key = "Control+" + key
		}
		if event.Alt {
			key = "Alt+" + key
		}
		if event.Shift {
			key = "Shift+" + key
		}
		if event.Meta {
			key = "Meta+" + key
		}
		encoded, modifiers := browserKey(key)
		err = chromedp.Run(opCtx, chromedp.KeyEvent(encoded, browserKeyOptions(key, modifiers)...))
	case "move", "down", "up", "wheel":
		mouseType := input.MouseMoved
		switch strings.ToLower(event.Kind) {
		case "down":
			mouseType = input.MousePressed
		case "up":
			mouseType = input.MouseReleased
		case "wheel":
			mouseType = input.MouseWheel
		}
		button := input.None
		switch strings.ToLower(event.Button) {
		case "left":
			button = input.Left
		case "middle":
			button = input.Middle
		case "right":
			button = input.Right
		}
		params := input.DispatchMouseEvent(mouseType, event.X, event.Y).WithButton(button).WithButtons(event.Buttons).WithClickCount(event.ClickCount)
		if mouseType == input.MouseWheel {
			params = params.WithDeltaX(event.DeltaX).WithDeltaY(event.DeltaY)
		}
		err = params.Do(targetCtx)
	default:
		return fmt.Errorf("browser: unsupported companion input %q", event.Kind)
	}
	if err != nil {
		return fmt.Errorf("browser: companion input: %w", err)
	}
	// Input targets the already-active companion page. Updating its MRU stamp
	// is enough; emitting a full tab-state event for every pointer move would
	// flood every local connection while changing no visible state.
	p.touch()
	return nil
}
