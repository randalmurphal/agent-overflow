package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxLocatorMatches = 1000
const maxLocatorBytes = 64 << 10
const maxBrowserInputBytes = 1 << 20

func (m *Manager) Locator(ctx context.Context, access Access, opts LocatorOptions) (LocatorResult, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return LocatorResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	timeout, err := boundedTimeout(opts.TimeoutMS)
	if err != nil {
		return LocatorResult{}, err
	}
	opCtx, cancel := operationContext(ctx, p.ctx, timeout)
	defer cancel()

	action := strings.ToLower(strings.TrimSpace(opts.Action))
	if action == "" {
		action = "count"
	}
	if err := validateLocatorOptions(opts, action); err != nil {
		return LocatorResult{}, err
	}
	var matches []LocatorMatch
	if action == "wait" {
		matches, err = m.waitLocator(opCtx, p, opts.Locator, opts.WaitState)
	} else {
		matches, err = m.resolveLocator(opCtx, p, opts.Locator, opts.Attribute)
	}
	if err != nil {
		return LocatorResult{}, fmt.Errorf("browser: locator: %w", err)
	}
	if mutatingLocatorAction(action) && (len(matches) == 0 || (!opts.Force && len(matches) == 1 && (!matches[0].Visible || !matches[0].Enabled))) {
		matches, err = m.waitActionLocator(opCtx, p, opts.Locator, opts.Force)
		if err != nil {
			return LocatorResult{}, err
		}
	}
	for i := range matches {
		matches[i].NodeID = p.rememberNode(nodeReference{Selector: matches[i].Selector, Frames: append([]string(nil), opts.Locator.Frames...), Tag: matches[i].Tag, Text: matches[i].Text})
	}
	result := LocatorResult{Count: len(matches)}
	p.downloadMu.Lock()
	downloadAfter := p.downloadSeq
	p.downloadMu.Unlock()
	var before navigationMark
	if opts.ExpectNavigation {
		before, _ = p.driver.NavigationMark(opCtx)
	}

	switch action {
	case "count":
	case "all":
		result.Matches = matches
	case "all_text_contents":
		result.Values = make([]string, 0, len(matches))
		for _, match := range matches {
			result.Values = append(result.Values, match.Text)
		}
	case "wait":
	case "get_attribute":
		match, strictErr := strictMatch(matches)
		if strictErr != nil {
			return LocatorResult{}, strictErr
		}
		value, callErr := p.driver.ReadNode(opCtx, match, opts.Locator, "attribute", opts.Attribute)
		if callErr != nil {
			return LocatorResult{}, callErr
		}
		result.Value = value
	case "inner_text":
		result.Value, err = m.strictRead(opCtx, p, matches, opts.Locator, "innerText", "")
	case "text_content":
		result.Value, err = m.strictRead(opCtx, p, matches, opts.Locator, "textContent", "")
	case "is_enabled":
		result.Value, err = m.strictRead(opCtx, p, matches, opts.Locator, "enabled", "")
	case "is_visible":
		if len(matches) == 0 {
			result.Value = false
		} else {
			result.Value, err = m.strictRead(opCtx, p, matches, opts.Locator, "visible", "")
		}
	case "click", "double_click", "fill", "type", "press", "check", "uncheck", "set_checked", "select_option":
		err = m.performLocatorAction(opCtx, p, matches, opts)
	default:
		return LocatorResult{}, fmt.Errorf("browser: unsupported locator action %q", action)
	}
	if err != nil {
		return LocatorResult{}, err
	}
	if opts.ExpectNavigation {
		if err := m.waitForNavigation(opCtx, p, before, opts.URL, opts.WaitUntil); err != nil {
			return LocatorResult{}, fmt.Errorf("browser: expected navigation: %w", err)
		}
		result.Navigated = true
	}
	if opts.ExpectDownload {
		download, waitErr := waitDownloadPage(opCtx, p, downloadAfter)
		if waitErr != nil {
			return LocatorResult{}, waitErr
		}
		result.Download = &download
	}
	if mutatingLocatorAction(action) {
		m.captureLocalStorage(opCtx, p)
	}
	info, infoErr := m.finishPageOperation(opCtx, p)
	if infoErr != nil {
		return LocatorResult{}, infoErr
	}
	result.Page = info
	return result, nil
}

func validateLocatorOptions(opts LocatorOptions, action string) error {
	encoded, err := json.Marshal(opts.Locator)
	if err != nil || len(encoded) > maxLocatorBytes {
		return fmt.Errorf("browser: locator exceeds %d bytes", maxLocatorBytes)
	}
	if len(opts.Value) > maxBrowserInputBytes {
		return fmt.Errorf("browser: locator value exceeds %d bytes", maxBrowserInputBytes)
	}
	if len(opts.Values) > 100 || len(opts.Select) > 100 {
		return fmt.Errorf("browser: select options exceed 100 entries")
	}
	for _, value := range opts.Values {
		if len(value) > maxBrowserInputBytes {
			return fmt.Errorf("browser: select value exceeds %d bytes", maxBrowserInputBytes)
		}
	}
	if len(opts.Modifiers) > 5 {
		return fmt.Errorf("browser: modifiers exceed 5 entries")
	}
	if (opts.ExpectNavigation || opts.ExpectDownload) && !mutatingLocatorAction(action) {
		return fmt.Errorf("browser: navigation/download expectation requires an action")
	}
	if action == "get_attribute" && strings.TrimSpace(opts.Attribute) == "" {
		return fmt.Errorf("browser: attribute is required")
	}
	for _, selection := range opts.Select {
		count := 0
		if selection.Value != nil {
			count++
		}
		if selection.Label != nil {
			count++
		}
		if selection.Index != nil {
			count++
			if *selection.Index < 0 {
				return fmt.Errorf("browser: select index must be non-negative")
			}
		}
		if count != 1 {
			return fmt.Errorf("browser: each select descriptor requires exactly one of value, label, or index")
		}
	}
	return nil
}

// resolveLocator validates the locator shape before every engine resolution, so
// a poll loop cannot smuggle an unvalidated locator past the tool contract.
func (m *Manager) resolveLocator(ctx context.Context, p *managedPage, locator Locator, attribute string) ([]LocatorMatch, error) {
	if err := validateLocator(locator, 0); err != nil {
		return nil, err
	}
	return p.driver.ResolveLocator(ctx, locator, attribute)
}

func (m *Manager) waitActionLocator(ctx context.Context, p *managedPage, locator Locator, force bool) ([]LocatorMatch, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		matches, err := m.resolveLocator(ctx, p, locator, "")
		if err == nil {
			if len(matches) > 1 {
				return nil, fmt.Errorf("browser: strict locator resolved to %d elements; refine it or set locator.index after checking count", len(matches))
			}
			if len(matches) == 1 && (force || (matches[0].Visible && matches[0].Enabled)) {
				return matches, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("browser: wait for actionable locator: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func boundedTimeout(milliseconds int) (time.Duration, error) {
	if milliseconds < 0 || milliseconds > 30_000 {
		return 0, fmt.Errorf("browser: timeout must be between 0 and 30000 ms")
	}
	if milliseconds == 0 {
		return operationTimeout, nil
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func mutatingLocatorAction(action string) bool {
	switch action {
	case "click", "double_click", "fill", "type", "press", "check", "uncheck", "set_checked", "select_option":
		return true
	default:
		return false
	}
}

func strictMatch(matches []LocatorMatch) (LocatorMatch, error) {
	if len(matches) != 1 {
		return LocatorMatch{}, fmt.Errorf("browser: strict locator resolved to %d elements; refine it or set locator.index after checking count", len(matches))
	}
	return matches[0], nil
}

func (m *Manager) strictRead(ctx context.Context, p *managedPage, matches []LocatorMatch, locator Locator, kind, argument string) (any, error) {
	match, err := strictMatch(matches)
	if err != nil {
		return nil, err
	}
	return p.driver.ReadNode(ctx, match, locator, kind, argument)
}

func (m *Manager) waitLocator(ctx context.Context, p *managedPage, locator Locator, rawState string) ([]LocatorMatch, error) {
	state := strings.ToLower(strings.TrimSpace(rawState))
	if state == "" {
		state = "visible"
	}
	if state != "attached" && state != "detached" && state != "visible" && state != "hidden" {
		return nil, fmt.Errorf("state must be attached, detached, visible, or hidden")
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		matches, err := m.resolveLocator(ctx, p, locator, "")
		if err == nil {
			satisfied := false
			switch state {
			case "attached":
				satisfied = len(matches) > 0
			case "detached":
				satisfied = len(matches) == 0
			case "visible":
				for _, match := range matches {
					satisfied = satisfied || match.Visible
				}
			case "hidden":
				satisfied = len(matches) == 0
				if !satisfied {
					satisfied = true
					for _, match := range matches {
						satisfied = satisfied && !match.Visible
					}
				}
			}
			if satisfied {
				return matches, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for locator %s: %w", state, ctx.Err())
		case <-ticker.C:
		}
	}
}

// performLocatorAction resolves the tool's action vocabulary into the single
// policy-checked mutation the driver carries out. Strictness, actionability,
// checkability, and the select descriptor rules are all decided here.
func (m *Manager) performLocatorAction(ctx context.Context, p *managedPage, matches []LocatorMatch, opts LocatorOptions) error {
	match, err := strictMatch(matches)
	if err != nil {
		return err
	}
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	act := nodeAction{Kind: action, Value: opts.Value, Clicks: 1, Button: opts.Button, Modifiers: opts.Modifiers}
	switch action {
	case "click", "double_click":
		if !opts.Force && (!match.Visible || !match.Enabled) {
			return fmt.Errorf("browser: target is not visible and enabled")
		}
		act.Kind = "click"
		if action == "double_click" {
			act.Clicks = 2
		}
	case "type", "press", "fill":
	case "check", "uncheck", "set_checked":
		want := action == "check"
		if action == "set_checked" {
			if opts.Checked == nil {
				return fmt.Errorf("browser: checked is required")
			}
			want = *opts.Checked
		}
		if match.Checked == nil {
			return fmt.Errorf("browser: target is not checkable")
		}
		if *match.Checked == want {
			return nil
		}
		// Toggling is a plain click: no button, no modifiers, one press.
		act = nodeAction{Kind: "click", Clicks: 1}
	case "select_option":
		selections := append([]SelectArg(nil), opts.Select...)
		if len(selections) == 0 {
			for _, value := range opts.Values {
				chosen := value
				selections = append(selections, SelectArg{Value: &chosen})
			}
		}
		if len(selections) == 0 && opts.Value != "" {
			chosen := opts.Value
			selections = []SelectArg{{Value: &chosen}}
		}
		if len(selections) == 0 {
			return fmt.Errorf("browser: select_option requires value, values, or select descriptors")
		}
		act.Selections = selections
	default:
		return fmt.Errorf("browser: unsupported locator action")
	}
	return p.driver.ActOnNode(ctx, match, opts.Locator, act)
}

func validateLocator(locator Locator, depth int) error {
	if depth > 8 {
		return fmt.Errorf("locator nesting exceeds 8")
	}
	strategies := 0
	for _, value := range []string{locator.CSS, locator.Role, locator.Text, locator.Label, locator.Placeholder, locator.TestID} {
		if strings.TrimSpace(value) != "" {
			strategies++
		}
	}
	if strategies == 0 && locator.Scope == nil && len(locator.And) == 0 && len(locator.Or) == 0 {
		return fmt.Errorf("locator strategy is required")
	}
	if len(locator.Frames) > 8 || len(locator.And) > 8 || len(locator.Or) > 8 {
		return fmt.Errorf("locator collection exceeds 8 entries")
	}
	if depth > 0 && len(locator.Frames) > 0 {
		return fmt.Errorf("nested locators inherit the outer frame and cannot declare frames")
	}
	if locator.RegexFlags != "" && !locator.Regex {
		return fmt.Errorf("regex_flags requires regex")
	}
	seenFlags := map[rune]bool{}
	for _, flag := range locator.RegexFlags {
		if !strings.ContainsRune("imsu", flag) || seenFlags[flag] {
			return fmt.Errorf("regex_flags may contain each of i, m, s, or u once")
		}
		seenFlags[flag] = true
	}
	if locator.Index != nil && (*locator.Index < 0 || *locator.Index >= maxLocatorMatches) {
		return fmt.Errorf("locator index is out of range")
	}
	for _, nested := range append(append([]*Locator{locator.Scope, locator.Has, locator.HasNot}, locator.And...), locator.Or...) {
		if nested != nil {
			if err := validateLocator(*nested, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
