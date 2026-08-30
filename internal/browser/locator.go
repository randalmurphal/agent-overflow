package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
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
		matches, err = resolveLocator(opCtx, opts.Locator, opts.Attribute)
	}
	if err != nil {
		return LocatorResult{}, fmt.Errorf("browser: locator: %w", err)
	}
	if mutatingLocatorAction(action) && (len(matches) == 0 || (!opts.Force && len(matches) == 1 && (!matches[0].Visible || !matches[0].Enabled))) {
		matches, err = waitActionLocator(opCtx, opts.Locator, opts.Force)
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
	beforeURL := ""
	var beforeLoader cdp.LoaderID
	if opts.ExpectNavigation {
		_ = chromedp.Run(opCtx, chromedp.Location(&beforeURL))
		if tree, treeErr := page.GetFrameTree().Do(targetCommandContext(opCtx)); treeErr == nil && tree != nil && tree.Frame != nil {
			beforeLoader = tree.Frame.LoaderID
		}
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
		value, callErr := nodeRead(opCtx, match, opts.Locator, "attribute", opts.Attribute)
		if callErr != nil {
			return LocatorResult{}, callErr
		}
		result.Value = value
	case "inner_text":
		result.Value, err = strictRead(opCtx, matches, opts.Locator, "innerText", "")
	case "text_content":
		result.Value, err = strictRead(opCtx, matches, opts.Locator, "textContent", "")
	case "is_enabled":
		result.Value, err = strictRead(opCtx, matches, opts.Locator, "enabled", "")
	case "is_visible":
		if len(matches) == 0 {
			result.Value = false
		} else {
			result.Value, err = strictRead(opCtx, matches, opts.Locator, "visible", "")
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
		if err := waitForNavigation(opCtx, p, beforeURL, beforeLoader, opts.URL, opts.WaitUntil); err != nil {
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

func waitActionLocator(ctx context.Context, locator Locator, force bool) ([]LocatorMatch, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		matches, err := resolveLocator(ctx, locator, "")
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

func strictRead(ctx context.Context, matches []LocatorMatch, locator Locator, kind, argument string) (any, error) {
	match, err := strictMatch(matches)
	if err != nil {
		return nil, err
	}
	return nodeRead(ctx, match, locator, kind, argument)
}

func (m *Manager) waitLocator(ctx context.Context, _ *managedPage, locator Locator, rawState string) ([]LocatorMatch, error) {
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
		matches, err := resolveLocator(ctx, locator, "")
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

func (m *Manager) performLocatorAction(ctx context.Context, p *managedPage, matches []LocatorMatch, opts LocatorOptions) error {
	match, err := strictMatch(matches)
	if err != nil {
		return err
	}
	root, err := locatorFrameRoot(ctx, opts.Locator.Frames)
	if err != nil {
		return err
	}
	var nodes []*cdp.Node
	if err := chromedp.Run(ctx, chromedp.Nodes(match.Selector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(root))); err != nil {
		return fmt.Errorf("browser: resolve action target: %w", err)
	}
	if len(nodes) != 1 {
		return fmt.Errorf("browser: locator became stale; take a fresh snapshot and retry")
	}
	node := nodes[0]
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	switch action {
	case "click", "double_click":
		if !opts.Force && (!match.Visible || !match.Enabled) {
			return fmt.Errorf("browser: target is not visible and enabled")
		}
		mouseOpts, optErr := mouseOptions(opts.Button, opts.Modifiers)
		if optErr != nil {
			return optErr
		}
		if action == "double_click" {
			mouseOpts = append(mouseOpts, chromedp.ClickCount(2))
		}
		return chromedp.Run(ctx, chromedp.MouseClickNode(node, mouseOpts...))
	case "type":
		return chromedp.Run(ctx, chromedp.KeyEventNode(node, opts.Value))
	case "press":
		key, modifiers := browserKey(opts.Value)
		if key == "" {
			return fmt.Errorf("browser: key is required")
		}
		return chromedp.Run(ctx, chromedp.KeyEventNode(node, key, browserKeyOptions(opts.Value, modifiers)...))
	case "fill":
		return callElementFunction(ctx, node, fmt.Sprintf(`function(){const v=%s;if(!(this instanceof HTMLInputElement||this instanceof HTMLTextAreaElement||this.isContentEditable))throw new Error("element is not fillable");if(this.isContentEditable)this.textContent=v;else{const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set;setter?setter.call(this,v):this.value=v}this.dispatchEvent(new InputEvent("input",{bubbles:true,inputType:"insertText",data:v}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, jsonString(opts.Value)))
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
		if *match.Checked != want {
			return chromedp.Run(ctx, chromedp.MouseClickNode(node))
		}
		return nil
	case "select_option":
		selections := append([]SelectArg(nil), opts.Select...)
		if len(selections) == 0 {
			for _, value := range opts.Values {
				copy := value
				selections = append(selections, SelectArg{Value: &copy})
			}
		}
		if len(selections) == 0 && opts.Value != "" {
			copy := opts.Value
			selections = []SelectArg{{Value: &copy}}
		}
		if len(selections) == 0 {
			return fmt.Errorf("browser: select_option requires value, values, or select descriptors")
		}
		encoded, _ := json.Marshal(selections)
		return callElementFunction(ctx, node, fmt.Sprintf(`function(){if(!(this instanceof HTMLSelectElement))throw new Error("element is not a select");const specs=%s, multi=this.multiple;for(const o of this.options)o.selected=false;for(const s of specs){let o=null;if(s.value!==undefined)o=[...this.options].find(x=>x.value===s.value);else if(s.label!==undefined)o=[...this.options].find(x=>x.label===s.label);else if(s.index!==undefined)o=this.options[s.index];if(!o)throw new Error("option not found");o.selected=true;if(!multi)break}this.dispatchEvent(new Event("input",{bubbles:true}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, string(encoded)))
	default:
		return fmt.Errorf("browser: unsupported locator action")
	}
}

func resolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error) {
	if err := validateLocator(locator, 0); err != nil {
		return nil, err
	}
	root, err := locatorFrameRoot(ctx, locator.Frames)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(locator)
	fn := fmt.Sprintf(locatorResolverJS, string(encoded), jsonString(attribute), maxLocatorMatches)
	obj, err := dom.ResolveNode().WithNodeID(root.NodeID).Do(targetCommandContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("resolve frame root: %w", err)
	}
	defer func() { _ = cdpruntime.ReleaseObject(obj.ObjectID).Do(targetCommandContext(ctx)) }()
	remote, exception, err := cdpruntime.CallFunctionOn(fn).WithObjectID(obj.ObjectID).WithReturnByValue(true).WithAwaitPromise(true).Do(targetCommandContext(ctx))
	if err != nil {
		return nil, err
	}
	if exception != nil {
		return nil, fmt.Errorf("%s", exception.Text)
	}
	var matches []LocatorMatch
	if remote == nil || len(remote.Value) == 0 {
		return nil, fmt.Errorf("locator returned no result")
	}
	if len(remote.Value) > maxLocatorResultBytes {
		return nil, fmt.Errorf("locator result exceeds %d bytes", maxLocatorResultBytes)
	}
	if err := json.Unmarshal(remote.Value, &matches); err != nil {
		return nil, fmt.Errorf("decode matches: %w", err)
	}
	for i := range matches {
		matches[i].FrameDepth = len(locator.Frames)
	}
	return matches, nil
}

func locatorFrameRoot(ctx context.Context, frames []string) (*cdp.Node, error) {
	var roots []*cdp.Node
	if err := chromedp.Run(ctx, chromedp.Nodes("html", &roots, chromedp.ByQueryAll, chromedp.AtLeast(0))); err != nil || len(roots) != 1 {
		if err == nil {
			err = fmt.Errorf("document root unavailable")
		}
		return nil, err
	}
	root := roots[0]
	for _, selector := range frames {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			return nil, fmt.Errorf("browser: empty frame selector")
		}
		var frameNodes []*cdp.Node
		if err := chromedp.Run(ctx, chromedp.Nodes(selector, &frameNodes, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(root))); err != nil {
			return nil, err
		}
		if len(frameNodes) != 1 {
			return nil, fmt.Errorf("browser: frame selector %q resolved to %d elements", selector, len(frameNodes))
		}
		var frameRoots []*cdp.Node
		if err := chromedp.Run(ctx, chromedp.Nodes("html", &frameRoots, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(frameNodes[0]))); err != nil {
			return nil, err
		}
		if len(frameRoots) != 1 {
			return nil, fmt.Errorf("browser: frame %q is not ready or accessible", selector)
		}
		root = frameRoots[0]
	}
	return root, nil
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

func nodeRead(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error) {
	root, err := locatorFrameRoot(ctx, locator.Frames)
	if err != nil {
		return nil, err
	}
	var nodes []*cdp.Node
	if err := chromedp.Run(ctx, chromedp.Nodes(match.Selector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(root))); err != nil || len(nodes) != 1 {
		return nil, fmt.Errorf("browser: locator became stale")
	}
	var fn string
	switch kind {
	case "attribute":
		fn = fmt.Sprintf(`function(){const v=this.getAttribute(%s);return v===null?null:v.slice(0,%d)}`, jsonString(argument), maxEvaluateBytes)
	case "innerText":
		fn = fmt.Sprintf(`function(){return String(this.innerText||"").slice(0,%d)}`, maxEvaluateBytes)
	case "textContent":
		fn = fmt.Sprintf(`function(){const v=this.textContent;return v===null?null:String(v).slice(0,%d)}`, maxEvaluateBytes)
	case "enabled":
		fn = `function(){return !(this.disabled||this.getAttribute("aria-disabled")==="true")}`
	case "visible":
		fn = `function(){const r=this.getBoundingClientRect(),s=getComputedStyle(this);return !!(r.width&&r.height&&s.display!=="none"&&s.visibility!=="hidden"&&s.visibility!=="collapse")}`
	default:
		return nil, fmt.Errorf("browser: invalid locator read")
	}
	return callElementFunctionValue(ctx, nodes[0], fn)
}

func callElementFunction(ctx context.Context, node *cdp.Node, fn string) error {
	_, err := callElementFunctionValue(ctx, node, fn)
	return err
}

func callElementFunctionValue(ctx context.Context, node *cdp.Node, fn string) (any, error) {
	obj, err := dom.ResolveNode().WithNodeID(node.NodeID).Do(targetCommandContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cdpruntime.ReleaseObject(obj.ObjectID).Do(targetCommandContext(ctx)) }()
	remote, exception, err := cdpruntime.CallFunctionOn(fn).WithObjectID(obj.ObjectID).WithReturnByValue(true).WithUserGesture(true).Do(targetCommandContext(ctx))
	if err != nil {
		return nil, err
	}
	if exception != nil {
		return nil, fmt.Errorf("browser: element action: %s", exception.Text)
	}
	if remote == nil || len(remote.Value) == 0 {
		return nil, nil
	}
	if len(remote.Value) > maxEvaluateBytes {
		return nil, fmt.Errorf("browser: element result exceeds %d bytes", maxEvaluateBytes)
	}
	var value any
	if err := json.Unmarshal(remote.Value, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func mouseOptions(button string, modifiers []string) ([]chromedp.MouseOption, error) {
	opts := []chromedp.MouseOption{}
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		opts = append(opts, chromedp.ButtonLeft)
	case "right":
		opts = append(opts, chromedp.ButtonRight)
	case "middle":
		opts = append(opts, chromedp.ButtonMiddle)
	default:
		return nil, fmt.Errorf("browser: button must be left, right, or middle")
	}
	mods := make([]input.Modifier, 0, len(modifiers))
	for _, modifier := range modifiers {
		switch strings.ToLower(strings.TrimSpace(modifier)) {
		case "alt":
			mods = append(mods, input.ModifierAlt)
		case "control", "ctrl":
			mods = append(mods, input.ModifierCtrl)
		case "meta", "command", "cmd":
			mods = append(mods, input.ModifierMeta)
		case "shift":
			mods = append(mods, input.ModifierShift)
		case "controlormeta":
			if runtime.GOOS == "darwin" {
				mods = append(mods, input.ModifierMeta)
			} else {
				mods = append(mods, input.ModifierCtrl)
			}
		default:
			return nil, fmt.Errorf("browser: unsupported modifier %q", modifier)
		}
	}
	if len(mods) > 0 {
		opts = append(opts, chromedp.ButtonModifiers(mods...))
	}
	return opts, nil
}

func jsonString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

const locatorResolverJS = `function(){
const query=%s, requestedAttribute=%s, cap=%d, root=this;
const clean=s=>(s||"").replace(/\s+/g," ").trim();
const match=(actual,want,exact,regex,flags)=>regex?new RegExp(want,flags||"").test(String(actual||"")):(exact?clean(actual)===clean(want):clean(actual).toLowerCase().includes(clean(want).toLowerCase()));
const visible=el=>{const r=el.getBoundingClientRect(),s=getComputedStyle(el);return !!(r.width&&r.height&&s.display!=="none"&&s.visibility!=="hidden"&&s.visibility!=="collapse")};
const role=el=>{const explicit=(el.getAttribute("role")||"").split(/\s+/)[0];if(explicit)return explicit;const tag=el.tagName;if(tag==="A"&&el.hasAttribute("href"))return "link";if(tag==="SELECT")return el.multiple||el.size>1?"listbox":"combobox";if(tag==="TH")return el.getAttribute("scope")==="row"?"rowheader":"columnheader";if(/^H[1-6]$/.test(tag))return "heading";if(el.isContentEditable)return "textbox";return ({BUTTON:"button",TEXTAREA:"textbox",SUMMARY:"button",IMG:"img",TABLE:"table",TR:"row",TD:"cell",THEAD:"rowgroup",TBODY:"rowgroup",TFOOT:"rowgroup",UL:"list",OL:"list",MENU:"list",LI:"listitem",NAV:"navigation",MAIN:"main",ARTICLE:"article",ASIDE:"complementary",DIALOG:"dialog",DETAILS:"group",FIELDSET:"group",OPTION:"option",PROGRESS:"progressbar",METER:"meter",OUTPUT:"status",DT:"term",DD:"definition"}[tag]||(tag==="INPUT"?({button:"button",submit:"button",reset:"button",image:"button",checkbox:"checkbox",radio:"radio",range:"slider",search:"searchbox",email:"textbox",tel:"textbox",text:"textbox",url:"textbox",number:"spinbutton"}[el.type]||""):""))};
const name=el=>{const ids=(el.getAttribute("aria-labelledby")||"").split(/\s+/).filter(Boolean),labelled=ids.map(id=>el.ownerDocument.getElementById(id)?.textContent||"").join(" ");return clean(el.getAttribute("aria-label")||labelled||el.labels?.[0]?.innerText||el.alt||el.title||el.innerText||el.value)};
const cssPath=el=>{if(el.id&&el.id.length<=256)return "#"+CSS.escape(el.id);const parts=[];while(el&&el.nodeType===1&&el!==root.parentElement&&parts.length<12){let p=el.tagName.toLowerCase();const tid=el.getAttribute("data-testid");if(tid&&tid.length<=256){p+='[data-testid="'+CSS.escape(tid)+'"]';parts.unshift(p);break}const parent=el.parentElement;if(parent){const peers=[...parent.children].filter(x=>x.tagName===el.tagName);if(peers.length>1)p+=":nth-of-type("+(peers.indexOf(el)+1)+")"}parts.unshift(p);if(el===root)break;el=parent}return parts.join(">");};
function core(q, within){
 let bases=q.scope?resolve(q.scope,within):[within], out=[];
 for(const base of bases){let selector=q.css||"*", nodes;try{nodes=[...base.querySelectorAll(selector)];if(base.matches?.(selector))nodes.unshift(base)}catch(e){throw new Error("invalid CSS selector: "+e.message)}
  for(const el of nodes){if(out.length>=cap)break;const r=role(el),n=name(el),txt=clean(el.textContent),inn=clean(el.innerText);
   if(q.role&&!match(r,q.role,true,q.regex,q.regex_flags))continue;if(q.name&&!match(n,q.name,q.exact,q.regex,q.regex_flags))continue;if(q.text&&!match(inn||txt,q.text,q.exact,q.regex,q.regex_flags))continue;
   if(q.label&&(!("labels" in el||el.hasAttribute("aria-label")||el.hasAttribute("aria-labelledby"))||!match(n,q.label,q.exact,q.regex,q.regex_flags)))continue;if(q.placeholder&&!match(el.getAttribute("placeholder"),q.placeholder,q.exact,q.regex,q.regex_flags))continue;if(q.test_id&&el.getAttribute("data-testid")!==q.test_id)continue;
   if(q.has_text&&!match(txt,q.has_text,false,q.regex,q.regex_flags))continue;if(q.has_not_text&&match(txt,q.has_not_text,false,q.regex,q.regex_flags))continue;if(q.visible!==undefined&&visible(el)!==q.visible)continue;
   if(q.has&&resolve(q.has,el).length===0)continue;if(q.has_not&&resolve(q.has_not,el).length>0)continue;out.push(el)
  }
 }
 if(q.text&&!q.css&&!q.role&&!q.label&&!q.placeholder&&!q.test_id)out=out.filter(el=>![...el.children].some(ch=>out.includes(ch)&&match(clean(ch.innerText)||clean(ch.textContent),q.text,q.exact,q.regex,q.regex_flags)));
 return [...new Set(out)];
}
 function resolve(q,within){let out=core(q,within);for(const a of q.and||[]){const set=new Set(resolve(a,within));out=out.filter(x=>set.has(x))}for(const o of q.or||[])out.push(...resolve(o,within));out=[...new Set(out)];if(q.index!==undefined)out=out[q.index]?[out[q.index]]:[];return out.slice(0,cap)}
return resolve(query,root).map(el=>{const checked=("checked" in el)?!!el.checked:(el.getAttribute("aria-checked")!==null?el.getAttribute("aria-checked")==="true":null);return {selector:cssPath(el),tag:el.tagName.toLowerCase(),role:role(el),name:name(el).slice(0,1000),text:(el.textContent||"").slice(0,4096),innerText:(el.innerText||"").slice(0,4096),visible:visible(el),enabled:!(el.disabled||el.getAttribute("aria-disabled")==="true"),checked,value:(el.value||"").slice(0,16000),attribute:requestedAttribute?String(el.getAttribute(requestedAttribute)||"").slice(0,4096):undefined}})
}`
