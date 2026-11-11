package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The WebKit driver's whole vocabulary, expressed as JavaScript.
//
// WebKitGTK has no remote-debugging protocol: every page operation the tools
// perform is one `webkit_web_view_call_async_javascript_function` call. This
// file builds those function bodies. It is deliberately TAG-FREE and free of
// cgo, so the builders are compiled and unit-tested on every platform while
// only the engine plumbing beside them is Linux-only.
//
// Two rules hold throughout:
//   - A body is the body of an ASYNC function. WebKit awaits its return value,
//     so `return <promise>` is how a promise result is resolved.
//   - Node resolution happens IN the page. CDP addresses a node by a remote
//     object id; WebKit has no such handle, so every element operation
//     re-resolves the frame chain and the selector, and a selector that no
//     longer resolves to exactly one element is a stale locator — the same
//     answer the CDP driver gives.

// webkitConsoleHandler is the script message handler the injected capture
// script posts to. It must be a bare JS identifier: it is spelled into the
// script, into the handler registration, and into the signal name the C side
// connects, and those three must agree.
const webkitConsoleHandler = "aoConsole"

// webkitFrameRootJS defines `aoRoot(frames)`, the frame-chain walk every
// element operation starts from. Frames are addressed by the same CSS
// selectors the CDP driver walks with DOM nodes; the difference is that JS can
// only reach a SAME-ORIGIN frame's document, which is a documented parity note
// for the native driver.
const webkitFrameRootJS = `const aoRoot=(frames)=>{let doc=document;for(const sel of frames){const raw=String(sel||"").trim();if(!raw)throw new Error("browser: empty frame selector");const found=doc.querySelectorAll(raw);if(found.length!==1)throw new Error("browser: frame selector "+JSON.stringify(raw)+" resolved to "+found.length+" elements");const inner=found[0].contentDocument;if(!inner||!inner.documentElement)throw new Error("browser: frame "+JSON.stringify(raw)+" is not ready or accessible");doc=inner}return doc.documentElement};`

// webkitElementJS defines `aoElement(frames, selector)`, which resolves the one
// element an already-matched locator names, or throws the stale-locator error.
const webkitElementJS = webkitFrameRootJS + `const aoElement=(frames,selector)=>{const root=aoRoot(frames),found=[...root.querySelectorAll(selector)];if(root.matches?.(selector))found.unshift(root);if(found.length!==1)throw new Error("browser: locator became stale; take a fresh snapshot and retry");return found[0]};`

// webkitLocatorResolveScript resolves a locator to its matches, capping the
// serialized result the same way the CDP driver does.
func webkitLocatorResolveScript(locator Locator, attribute string) string {
	return fmt.Sprintf(`%sconst root=aoRoot(%s);return (%s).call(root);`,
		webkitFrameRootJS, webkitFrameList(locator.Frames), locatorResolverFunction(locator, attribute))
}

// webkitFrameList renders a frame chain as a JS array literal. An unframed
// locator must still cross as `[]`: `null` is not iterable, and the frame walk
// would throw before it ever reached the selector.
func webkitFrameList(frames []string) string {
	if len(frames) == 0 {
		return "[]"
	}
	encoded, _ := json.Marshal(frames)
	return string(encoded)
}

// webkitElementCallScript runs one shared element-function body against the
// element a locator match names.
func webkitElementCallScript(frames []string, selector, fn string) string {
	return fmt.Sprintf(`%sconst el=aoElement(%s,%s);return (%s).call(el);`,
		webkitElementJS, webkitFrameList(frames), jsonString(selector), fn)
}

// webkitNodeActScript renders one policy-checked locator mutation. click,
// type, and press are the untrusted JS tier: `element.click()` rather than a
// constructed MouseEvent, because it runs the full activation-behaviour path
// (spike item 4), and focus + value + input/change for text.
func webkitNodeActScript(frames []string, selector string, act nodeAction) (string, error) {
	var fn string
	switch act.Kind {
	case "click":
		fn = webkitClickFunction(act.Clicks, act.Button, act.Modifiers)
	case "type":
		fn = webkitTypeFunction(act.Value, false)
	case "press":
		if !chordHasKey(act.Value) {
			return "", fmt.Errorf("browser: key is required")
		}
		fn = webkitPressFunction(act.Value)
	case "fill":
		fn = nodeFillFunction(act.Value)
	case "select_option":
		fn = nodeSelectOptionFunction(act.Selections)
	default:
		return "", fmt.Errorf("browser: unsupported locator action")
	}
	return webkitElementCallScript(frames, selector, fn), nil
}

// webkitClickFunction drives a click through the element's own activation
// behaviour. A plain left single click is `element.click()`; anything carrying
// a button, modifiers, or a second click needs the pointer/mouse event
// sequence a site listens for, still untrusted.
func webkitClickFunction(clicks int, button string, modifiers []string) string {
	if clicks < 1 {
		clicks = 1
	}
	if clicks == 1 && webkitMouseButton(button) == 0 && len(modifiers) == 0 {
		return `function(){this.scrollIntoView({block:"center",behavior:"instant"});this.click()}`
	}
	init, _ := json.Marshal(map[string]any{
		"bubbles":    true,
		"cancelable": true,
		"button":     webkitMouseButton(button),
		"buttons":    1 << webkitMouseButton(button),
		"detail":     clicks,
		"altKey":     webkitHasModifier(modifiers, "alt"),
		"ctrlKey":    webkitHasModifier(modifiers, "control"),
		"metaKey":    webkitHasModifier(modifiers, "meta"),
		"shiftKey":   webkitHasModifier(modifiers, "shift"),
	})
	return fmt.Sprintf(`function(){this.scrollIntoView({block:"center",behavior:"instant"});const init=%s;for(let i=1;i<=%d;i++){this.dispatchEvent(new MouseEvent("mousedown",{...init,detail:i}));this.dispatchEvent(new MouseEvent("mouseup",{...init,detail:i}));this.dispatchEvent(new MouseEvent("click",{...init,detail:i}))}if(%d===2)this.dispatchEvent(new MouseEvent("dblclick",init));}`, string(init), clicks, clicks)
}

// webkitTypeFunction focuses an element, optionally clears it, and sets its
// value with the input/change events a framework listens for.
func webkitTypeFunction(text string, clear bool) string {
	clearJS := ""
	if clear {
		clearJS = `if(this.isContentEditable)this.textContent="";else{const c=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set;c?c.call(this,""):this.value=""}`
	}
	return fmt.Sprintf(`function(){this.scrollIntoView({block:"center",behavior:"instant"});this.focus?.();%sconst v=%s;if(this.isContentEditable)this.textContent=(this.textContent||"")+v;else{const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set,next=(this.value||"")+v;setter?setter.call(this,next):this.value=next}for(const ch of v){this.dispatchEvent(new KeyboardEvent("keydown",{bubbles:true,key:ch}));this.dispatchEvent(new KeyboardEvent("keyup",{bubbles:true,key:ch}))}this.dispatchEvent(new InputEvent("input",{bubbles:true,inputType:"insertText",data:v}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, clearJS, jsonString(text))
}

// webkitPressFunction dispatches one key chord at an element. Enter on a form
// control submits, matching what a real keypress does in Chrome.
func webkitPressFunction(raw string) string {
	key, modifiers := webkitKeyChord(raw)
	init, _ := json.Marshal(map[string]any{
		"bubbles":    true,
		"cancelable": true,
		"key":        key,
		"altKey":     modifiers["alt"],
		"ctrlKey":    modifiers["control"],
		"metaKey":    modifiers["meta"],
		"shiftKey":   modifiers["shift"],
	})
	return fmt.Sprintf(`function(){this.focus?.();const init=%s;const down=new KeyboardEvent("keydown",init);const proceed=this.dispatchEvent(down);if(proceed&&init.key.length===1&&!init.ctrlKey&&!init.metaKey){if(this.isContentEditable)this.textContent=(this.textContent||"")+init.key;else if("value" in this){const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set,next=(this.value||"")+init.key;setter?setter.call(this,next):this.value=next}this.dispatchEvent(new InputEvent("input",{bubbles:true,inputType:"insertText",data:init.key}))}this.dispatchEvent(new KeyboardEvent("keyup",init));if(proceed&&init.key==="Enter"&&this.form&&typeof this.form.requestSubmit==="function")this.form.requestSubmit();}`, string(init))
}

// webkitEvaluate evaluates an evaluation's source through a page's body
// evaluator, which awaits the body's return value. Both WebKit engines share
// it because both hand the body to the same JavaScriptCore, whose error for a
// body that does not compile both glues report as its "SyntaxError: ..."
// message. No other error is one: the runner catches what the code throws,
// and the body awaits the runner under a catch with the runner's last answer.
// Returning the runner's promise instead would leave adopting it, which reads
// the page's Promise.prototype, outside any catch. WebKit's own completion
// reads the body's promise the same way; when the page makes that throw,
// WebKit fails the call with its own error.
func webkitEvaluate(ctx context.Context, evalBody func(context.Context, string) (json.RawMessage, error), source string) (json.RawMessage, error) {
	raw, err := evalBody(ctx, "try {\nreturn await "+source+";\n} catch (_) {\nreturn "+evaluateUndescribedAnswer+";\n}")
	if err != nil {
		if message := err.Error(); strings.HasPrefix(message, "SyntaxError") {
			return nil, &evaluateSyntaxError{message: message}
		}
		return nil, err
	}
	return evaluationAnswer(raw)
}

// webkitSelectorClickScript is the `browser_click` tool's selector path.
func webkitSelectorClickScript(selector string) string {
	return fmt.Sprintf(`%sconst el=aoElement([],%s);return (%s).call(el);`, webkitElementJS, jsonString(selector), webkitClickFunction(1, "", nil))
}

// webkitSelectorTypeScript is the `browser_type` tool's selector path.
func webkitSelectorTypeScript(selector, text string, clear bool) string {
	return fmt.Sprintf(`%sconst el=aoElement([],%s);return (%s).call(el);`, webkitElementJS, jsonString(selector), webkitTypeFunction(text, clear))
}

// webkitFocusedPressScript presses one chord at whatever currently has focus,
// which is what `browser_press` means with no selector.
func webkitFocusedPressScript(raw string) string {
	return fmt.Sprintf(`const el=document.activeElement||document.body;if(!el)throw new Error("browser: page has no focusable target");return (%s).call(el);`, webkitPressFunction(raw))
}

// webkitTypeTextScript types at the current focus with no selector, the
// `browser_dom` type action.
func webkitTypeTextScript(text string) string {
	return fmt.Sprintf(`const el=document.activeElement||document.body;if(!el)throw new Error("browser: page has no focusable target");return (%s).call(el);`, webkitTypeFunction(text, false))
}

// webkitScrollScript scrolls a selector, or the window when it is empty.
func webkitScrollScript(selector string, x, y float64) string {
	return fmt.Sprintf(`const s=%s;const el=s?document.querySelector(s):window;if(!el)throw new Error("selector not found");el.scrollBy({left:%f,top:%f,behavior:"instant"});return true;`, jsonString(strings.TrimSpace(selector)), x, y)
}

// webkitFindWorld is the isolated script world the find's selection
// housekeeping runs in, so nothing the page defines (an overridden
// getSelection or removeAllRanges) reaches it.
const webkitFindWorld = "ao-find"

// webkitFrameClearJS is the selection clearing both webkitClearSelectionScript
// and webkitFindFrameScript run, in webkitFindWorld. aoClear(w) drops w's
// selection and that of every frame in w.frames it can reach (same origin),
// recursively; a frame it cannot reach (a SecurityError) is left to the
// messages (webkitFindFrameScript). A failure other than an unreachable
// frame goes to aoFailures.
const webkitFrameClearJS = `const aoFailures=[];const aoReachable=(e)=>{if(e&&e.name==="SecurityError")return false;aoFailures.push(String(e&&e.message||e));return false;};const aoClear=(w)=>{let s,n=0;try{s=w.getSelection();}catch(e){return aoReachable(e);}try{if(s)s.removeAllRanges();}catch(e){aoFailures.push(String(e&&e.message||e));}try{n=w.frames.length;}catch(e){return aoReachable(e);}for(let i=0;i<n;i++){let f;try{f=w.frames[i];}catch(e){aoReachable(e);continue;}if(f)aoClear(f);}return true;};`

// webkitClearSelectionScript drops the selection that marks a find's match
// in the page and its frames; WKWebView's find starts at the selection and
// both engines mark the match with it. It runs in webkitFindWorld, in the
// top frame. The page and every frame it reaches through window.frames are
// cleared in the call, and any failure there fails the script, so the stop
// it serves fails and stays owed. Then every frame that announced itself to
// the top frame (webkitFindFrameScript) is told to clear, and each tells its
// own. Those are cleared in later tasks, not waited for, and answer nothing,
// so they cannot fail the stop. A late clear can only remove a new search's
// match in such a frame if that match is selected before the frame runs a
// task already queued when the stop answered, which the round trip of the
// next start leaves to timing below a single task; the clears are not
// ordered against it. Without the frame script (an engine that refused it)
// such a frame keeps its selection.
const webkitClearSelectionScript = webkitFrameClearJS + `aoClear(window);const aoState=globalThis.aoFindState;if(aoState&&typeof aoState.tell==="function")aoState.tell();if(aoFailures.length)throw new Error("browser: clear the selection: "+aoFailures.join("; "));return null;`

// webkitFindFrameScript is installed in every frame at document start, in
// webkitFindWorld, so it exists in a frame added or navigated after the page
// loaded and nothing the page defines reaches it.
//
// Frames are not enumerated: window.frames misses an iframe in a shadow
// root. Each frame below the top announces itself to its parent
// ({"ao-find": "child"}), and the parent keeps an announcing window only
// while it is its own direct child (source.parent === window) and not
// closed, once, and at most 256; the list is pruned whenever it is used. A
// frame obeys a clear ({"ao-find": "clear-selection"}) only from its
// parent: it clears itself and the frames it reaches (webkitFrameClearJS)
// and passes the clear to its own announced children. The top frame obeys
// no message; the stop script clears it and starts the chain
// (aoFindState.tell). A clear so only travels down and cannot cycle, and a
// frame can clear nothing above or beside it.
//
// It runs again in a window whose document is regenerated with
// document.open, which keeps the window and its globals but drops its
// listeners. So every run installs its listener, first removing the one an
// earlier run left if that survived, and announces to the parent again;
// the window keeps exactly one listener. The child list lives on
// aoFindState, the one global that outlasts a run, and carries over, each
// entry still checked when used.
//
// The listener is in the capture phase and added before any page script in
// the document runs, and WebKit calls a target's listeners in the order they
// were added whatever their world, so a page listener cannot run before it;
// it stops its own two message kinds from reaching the page. The exception
// is a listener a same-origin parent added to a frame's initial about:blank
// window that the frame's first document then reuses. A frame that runs no
// script (sandboxed without allow-scripts) cannot take part, and keeps its
// selection.
const webkitFindFrameScript = `(()=>{` + webkitFrameClearJS + `const aoMax=256;let aoState=globalThis.aoFindState;if(!aoState){aoState={kids:[],listener:null,tell:null,count:null};Object.defineProperty(globalThis,"aoFindState",{value:aoState});}const aoKids=aoState.kids;const aoMine=(w)=>{try{return !!w&&!w.closed&&w.parent===window;}catch(e){return false;}};const aoPrune=()=>{for(let i=aoKids.length-1;i>=0;i--)if(!aoMine(aoKids[i]))aoKids.splice(i,1);};const aoTell=()=>{aoPrune();for(const w of aoKids){try{w.postMessage({"ao-find":"clear-selection"},"*");}catch(e){}}};aoState.tell=aoTell;aoState.count=()=>{aoPrune();return aoKids.length;};if(aoState.listener)removeEventListener("message",aoState.listener,true);aoState.listener=(e)=>{const d=e.data;if(!d||typeof d!=="object")return;const kind=d["ao-find"];if(kind!=="child"&&kind!=="clear-selection")return;e.stopImmediatePropagation();const w=e.source;if(kind==="child"){if(!aoMine(w)||aoKids.includes(w))return;aoPrune();if(aoKids.length<aoMax)aoKids.push(w);return;}if(window.parent===window||w!==window.parent)return;aoClear(window);aoTell();};addEventListener("message",aoState.listener,true);if(window.parent!==window){try{window.parent.postMessage({"ao-find":"child"},"*");}catch(e){}}})();`

// webkitSelectionTextScript reads the page's current selection for the copy
// chord, matching the CDP driver's expression exactly.
const webkitSelectionTextScript = `const a=document.activeElement;if(a&&(a instanceof HTMLInputElement||a instanceof HTMLTextAreaElement)&&a.selectionStart!==null)return a.value.slice(a.selectionStart,a.selectionEnd);return String(getSelection()||"");`

// webkitVisibleScript reports whether a selector currently has a visible
// match. The Manager polls it; the driver never loops.
func webkitVisibleScript(selector string) string {
	return fmt.Sprintf(`const nodes=document.querySelectorAll(%s);for(const el of nodes){const r=el.getBoundingClientRect(),s=getComputedStyle(el);if(r.width&&r.height&&s.display!=="none"&&s.visibility!=="hidden"&&s.visibility!=="collapse")return true}return false;`, jsonString(selector))
}

// webkitInfoScript reads location and title in one call.
const webkitInfoScript = `return {url:location.href,title:document.title};`

// webkitPageStatusScript samples the load state a wait condition is evaluated
// against. WebKit has no network-idle judgement of its own, so the driver
// supplies that half from its own load bookkeeping.
const webkitPageStatusScript = `return {url:location.href,ready:document.readyState};`

// webkitNavigationMarkScript captures the page's navigation identity. WebKit
// has no loader id, so the history length stands in: it changes on a pushState
// or a same-URL navigation the URL alone would hide.
const webkitNavigationMarkScript = `return {url:location.href,loader:String(history.length)};`

// webkitPointerScript dispatches a raw pointer gesture at viewport
// coordinates. Untrusted, and it addresses whatever is at the point — which is
// what a coordinate gesture means.
func webkitPointerScript(opts PointerOptions) (string, error) {
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	button := webkitMouseButton(opts.Button)
	if button < 0 {
		return "", fmt.Errorf("browser: button must be left, right, middle, back, or forward")
	}
	if err := webkitValidateModifiers(opts.Modifiers); err != nil {
		return "", err
	}
	base := map[string]any{
		"bubbles": true, "cancelable": true, "button": button, "buttons": webkitButtonsMask(button),
		"altKey": webkitHasModifier(opts.Modifiers, "alt"), "ctrlKey": webkitHasModifier(opts.Modifiers, "control"),
		"metaKey": webkitHasModifier(opts.Modifiers, "meta"), "shiftKey": webkitHasModifier(opts.Modifiers, "shift"),
	}
	encodedBase, _ := json.Marshal(base)
	const pick = `const at=(x,y)=>{const el=document.elementFromPoint(x,y);if(!el)throw new Error("browser: no element at the requested point");return el};`
	switch action {
	case "click", "double_click":
		count := 1
		if action == "double_click" {
			count = 2
		}
		// Chrome's Input domain gets the DOM's button semantics for free; an
		// untrusted MouseEvent sequence has to spell them out: only the
		// primary button `click`s, every other button `auxclick`s, and the
		// secondary button raises `contextmenu` — the event a site's custom
		// menu listens for (verified missing on WKWebView 2026-09-03).
		return fmt.Sprintf(`%sconst init={...%s,clientX:%f,clientY:%f};const el=at(%f,%f);const primary=init.button===0;for(let i=1;i<=%d;i++){el.dispatchEvent(new MouseEvent("mousedown",{...init,detail:i}));el.dispatchEvent(new MouseEvent("mouseup",{...init,detail:i}));el.dispatchEvent(new MouseEvent(primary?"click":"auxclick",{...init,detail:i}))}if(init.button===2)el.dispatchEvent(new MouseEvent("contextmenu",{...init,detail:1}));if(%d===2&&primary)el.dispatchEvent(new MouseEvent("dblclick",{...init,detail:2}));return true;`,
			pick, string(encodedBase), opts.X, opts.Y, opts.X, opts.Y, count, count), nil
	case "move":
		return fmt.Sprintf(`%sconst el=at(%f,%f);el.dispatchEvent(new MouseEvent("mousemove",{...%s,buttons:0,clientX:%f,clientY:%f}));return true;`,
			pick, opts.X, opts.Y, string(encodedBase), opts.X, opts.Y), nil
	case "scroll":
		if err := validateScrollDelta(opts.ScrollX, opts.ScrollY); err != nil {
			return "", err
		}
		return fmt.Sprintf(`%sconst el=at(%f,%f);el.dispatchEvent(new WheelEvent("wheel",{...%s,buttons:0,clientX:%f,clientY:%f,deltaX:%f,deltaY:%f}));const target=el.closest("*");(target||window).scrollBy({left:%f,top:%f,behavior:"instant"});return true;`,
			pick, opts.X, opts.Y, string(encodedBase), opts.X, opts.Y, opts.ScrollX, opts.ScrollY, opts.ScrollX, opts.ScrollY), nil
	case "drag":
		if len(opts.Path) < 2 {
			return "", fmt.Errorf("browser: drag path requires at least two points")
		}
		for _, point := range opts.Path {
			if err := validatePoint(point); err != nil {
				return "", err
			}
		}
		path, _ := json.Marshal(opts.Path)
		// HTML5 drag-and-drop with an untrusted DataTransfer: the same sequence
		// Chrome's Input domain produces, minus the trust bit.
		return fmt.Sprintf(`%sconst path=%s,init=%s;const src=at(path[0].x,path[0].y);const dt=new DataTransfer();
src.dispatchEvent(new MouseEvent("mousedown",{...init,clientX:path[0].x,clientY:path[0].y}));
src.dispatchEvent(new DragEvent("dragstart",{...init,clientX:path[0].x,clientY:path[0].y,dataTransfer:dt}));
let last=src;for(let i=1;i<path.length;i++){const p=path[i],el=document.elementFromPoint(p.x,p.y)||last;if(el!==last){last.dispatchEvent(new DragEvent("dragleave",{...init,clientX:p.x,clientY:p.y,dataTransfer:dt}));el.dispatchEvent(new DragEvent("dragenter",{...init,clientX:p.x,clientY:p.y,dataTransfer:dt}))}el.dispatchEvent(new DragEvent("dragover",{...init,clientX:p.x,clientY:p.y,dataTransfer:dt}));last=el}
const end=path[path.length-1];last.dispatchEvent(new DragEvent("drop",{...init,clientX:end.x,clientY:end.y,dataTransfer:dt}));
src.dispatchEvent(new DragEvent("dragend",{...init,clientX:end.x,clientY:end.y,dataTransfer:dt}));
last.dispatchEvent(new MouseEvent("mouseup",{...init,buttons:0,clientX:end.x,clientY:end.y}));return true;`,
			pick, string(path), string(encodedBase)), nil
	default:
		return "", fmt.Errorf("browser: pointer action must be click, double_click, move, scroll, or drag")
	}
}

// webkitAssetFetchScript reads one asset through the page's own credentials
// and returns it base64-encoded with the content type the server sent. WebKit
// has no `Network.loadNetworkResource` equivalent, so the fetch happens in the
// page — which is also what makes the page's cookies apply.
func webkitAssetFetchScript(url string, limit int64) string {
	return fmt.Sprintf(`const res=await fetch(%s,{credentials:"include"});if(!res.ok)throw new Error("HTTP "+res.status);const buf=await res.arrayBuffer();if(buf.byteLength>%d)throw new Error("browser: asset exceeds bundle size limit");const bytes=new Uint8Array(buf);let binary="";const chunk=0x8000;for(let i=0;i<bytes.length;i+=chunk)binary+=String.fromCharCode.apply(null,bytes.subarray(i,i+chunk));return {contentType:res.headers.get("content-type")||"",base64:btoa(binary)};`, jsonString(url), limit)
}

// webkitConsoleCaptureScript is injected at document start in every frame. It
// is the WebKit half of what CDP's Runtime/Log domains report for free. It
// runs again in a window whose document is regenerated with document.open,
// which keeps the window's console but drops its listeners, so the state it
// keeps on the console (aoCapture) says which methods it already wrapped,
// and each run replaces its own listeners: an entry is reported once
// however many times it runs.
func webkitConsoleCaptureScript(handler string) string {
	return fmt.Sprintf(`(()=>{const post=window.webkit?.messageHandlers?.%s;if(!post)return;let st=console.aoCapture;if(!st){st={wrapped:{},error:null,rejection:null};Object.defineProperty(console,"aoCapture",{value:st});}const send=(level,parts)=>{try{post.postMessage(JSON.stringify({level,message:parts.map(v=>{try{return typeof v==="string"?v:(v instanceof Error?(v.stack||v.message):JSON.stringify(v))}catch(_){return String(v)}}).join(" ").slice(0,%d),url:location.href}))}catch(_){}};
for(const level of ["log","info","warning","warn","error","debug","trace"]){const original=console[level];if(typeof original!=="function"||st.wrapped[level]===original)continue;const wrapped=function(...args){send(level,args);return original.apply(this,args)};st.wrapped[level]=wrapped;console[level]=wrapped}
if(st.error)removeEventListener("error",st.error);if(st.rejection)removeEventListener("unhandledrejection",st.rejection);
st.error=e=>send("error",[e.message||String(e.error||"error")]);st.rejection=e=>send("error",["Unhandled promise rejection: "+String(e.reason)]);
addEventListener("error",st.error);addEventListener("unhandledrejection",st.rejection);})();`, handler, maxConsoleMessageBytes)
}

// webkitButtonsMask is MouseEvent.buttons for one pressed button. The
// bitmask does not follow MouseEvent.button's order: primary is 1, secondary
// (button 2) is 2, auxiliary (button 1) is 4, then back and forward.
func webkitButtonsMask(button int) int {
	switch button {
	case 0:
		return 1
	case 1:
		return 4
	case 2:
		return 2
	case 3:
		return 8
	case 4:
		return 16
	default:
		return 0
	}
}

func webkitMouseButton(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "left":
		return 0
	case "middle":
		return 1
	case "right":
		return 2
	case "back":
		return 3
	case "forward":
		return 4
	default:
		return -1
	}
}

func webkitValidateModifiers(raw []string) error {
	for _, value := range raw {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "alt", "control", "ctrl", "meta", "command", "cmd", "shift", "controlormeta":
		default:
			return fmt.Errorf("browser: unsupported modifier %q", value)
		}
	}
	return nil
}

// webkitHasModifier answers one canonical modifier name. `controlormeta`
// resolves to control: the native driver only runs on Linux and macOS builds
// of WebKit, and on Linux that chord is Control. macOS overrides it when that
// leg lands (spec §10).
func webkitHasModifier(modifiers []string, want string) bool {
	for _, value := range modifiers {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "alt", "option":
			if want == "alt" {
				return true
			}
		case "control", "ctrl", "controlormeta":
			if want == "control" {
				return true
			}
		case "meta", "command", "cmd":
			if want == "meta" {
				return true
			}
		case "shift":
			if want == "shift" {
				return true
			}
		}
	}
	return false
}

// webkitKeyChord splits a chord into the DOM `key` value and its modifiers,
// mirroring the CDP driver's vocabulary so one chord means one thing.
func webkitKeyChord(raw string) (string, map[string]bool) {
	modifiers := map[string]bool{"alt": false, "control": false, "meta": false, "shift": false}
	key := ""
	for _, part := range strings.Split(strings.TrimSpace(raw), "+") {
		trimmed := strings.TrimSpace(part)
		switch strings.ToLower(trimmed) {
		case "control", "ctrl", "controlormeta":
			modifiers["control"] = true
		case "shift":
			modifiers["shift"] = true
		case "alt", "option":
			modifiers["alt"] = true
		case "meta", "command", "cmd":
			modifiers["meta"] = true
		case "enter", "return":
			key = "Enter"
		case "tab":
			key = "Tab"
		case "escape", "esc":
			key = "Escape"
		case "backspace":
			key = "Backspace"
		case "delete":
			key = "Delete"
		case "arrowup", "up":
			key = "ArrowUp"
		case "arrowdown", "down":
			key = "ArrowDown"
		case "arrowleft", "left":
			key = "ArrowLeft"
		case "arrowright", "right":
			key = "ArrowRight"
		case "space":
			key = " "
		case "":
		default:
			key = trimmed
		}
	}
	return key, modifiers
}
