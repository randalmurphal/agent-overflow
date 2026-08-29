package compare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/cdpclient"
	"agent-overflow/internal/harnessclient"
)

func waitForPage(ctx context.Context, client *harnessclient.Client, requested, marker, origin string) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		info, err := client.Info(deadline)
		if err != nil {
			return "", err
		}
		if pageID, ok := selectOwnedPage(info.FrontendPages, requested, marker, origin); ok {
			return pageID, nil
		}
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("no frontend page registered for compare backend: %w", deadline.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func selectOwnedPage(pages []harnessclient.HarnessPageIdentity, requested, marker, origin string) (string, bool) {
	if requested != "" {
		for _, page := range pages {
			if page.PageID == requested && page.Marker == marker && sameOrigin(page.Origin, origin) {
				return requested, true
			}
		}
		return "", false
	}
	owned := make([]string, 0, len(pages))
	for _, page := range pages {
		if page.Marker == marker && sameOrigin(page.Origin, origin) {
			owned = append(owned, page.PageID)
		}
	}
	if len(owned) == 0 {
		return "", false
	}
	// Every candidate has the bootstrap's marker and origin, so each is
	// owned by this fresh backend. Pick one stably when a native shell
	// briefly exposes more than one document during reload.
	sort.Strings(owned)
	return owned[0], true
}

func bootstrapOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("backend bootstrap has invalid page url %q", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func sameOrigin(aRaw, bRaw string) bool {
	a, errA := url.Parse(aRaw)
	b, errB := url.Parse(bRaw)
	return errA == nil && errB == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func openLogicalPanes(ctx context.Context, client *harnessclient.Client, pageID string, panes []LogicalPane) error {
	first := true
	for _, pane := range panes {
		if pane.Kind != "" && pane.Kind != "thread" {
			continue
		}
		if pane.ThreadID == "" {
			continue
		}
		_, err := client.Call(ctx, "HarnessUIQuery", map[string]any{"v": 1, "kind": "open", "threadId": pane.ThreadID, "newPane": !first, "pageId": pageID})
		if err != nil {
			return fmt.Errorf("open pane %s: %w", pane.PaneID, err)
		}
		first = false
	}
	return nil
}

func waitForReplay(ctx context.Context, client *harnessclient.Client, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		raw, err := client.Call(deadline, "HarnessReplayStatus")
		if err != nil {
			return err
		}
		var status struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return err
		}
		switch status.State {
		case "done":
			return nil
		case "failed":
			return fmt.Errorf("capsule replay failed: %s", status.Error)
		case "stopped":
			return errors.New("capsule replay stopped")
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("capsule replay did not finish: %w", deadline.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func semanticText(ctx context.Context, client *harnessclient.Client, pageID string, panes []LogicalPane, _ *cdpclient.Conn) (string, error) {
	// The bridge is the semantic surface for every browser engine. CDP is
	// attached only to prove page ownership and never executes arbitrary page
	// code for correctness data.
	if err := waitForSemanticReady(ctx, client, pageID, panes); err != nil {
		return "", err
	}
	raw, err := client.Call(ctx, "HarnessUIQuery", map[string]any{"v": 1, "kind": "viewport", "textHead": 64 << 20, "settledMs": 300, "pageId": pageID})
	if err != nil {
		return "", err
	}
	return semanticTextFromViewport(raw)
}

func waitForSemanticReady(ctx context.Context, client *harnessclient.Client, pageID string, panes []LogicalPane) error {
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	want := make(map[string]struct{})
	for _, pane := range panes {
		if (pane.Kind == "" || pane.Kind == "thread") && pane.ThreadID != "" {
			want[pane.ThreadID] = struct{}{}
		}
	}
	for {
		raw, err := client.Call(deadline, "HarnessUIQuery", map[string]any{"v": 1, "kind": "viewport", "textHead": 1, "settledMs": 300, "pageId": pageID})
		if err != nil {
			return fmt.Errorf("wait for semantic viewport: %w", err)
		}
		var view struct {
			Settled bool `json:"settled"`
			Panes   []struct {
				ThreadID string `json:"threadId"`
			} `json:"panes"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			return fmt.Errorf("decode semantic readiness: %w", err)
		}
		seen := make(map[string]struct{}, len(view.Panes))
		for _, pane := range view.Panes {
			if pane.ThreadID != "" {
				seen[pane.ThreadID] = struct{}{}
			}
		}
		ready := view.Settled
		for threadID := range want {
			if _, ok := seen[threadID]; !ok {
				ready = false
				break
			}
		}
		if ready {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("semantic viewport did not become ready and settled: %w", deadline.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func semanticTextFromViewport(raw json.RawMessage) (string, error) {
	var view struct {
		Panes []struct {
			Rows []struct {
				TextHead string `json:"textHead"`
			} `json:"rows"`
		} `json:"panes"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return "", fmt.Errorf("decode semantic viewport: %w", err)
	}
	var b strings.Builder
	for _, pane := range view.Panes {
		for _, row := range pane.Rows {
			if strings.HasSuffix(row.TextHead, "…") {
				return "", errors.New("semantic viewport text is truncated; the full-text gate cannot be claimed")
			}
			b.WriteString(row.TextHead)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
