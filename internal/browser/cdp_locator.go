package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func (p *cdpPage) ResolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error) {
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

func (p *cdpPage) ReadNode(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error) {
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

func (p *cdpPage) ActOnNode(ctx context.Context, match LocatorMatch, locator Locator, act nodeAction) error {
	root, err := locatorFrameRoot(ctx, locator.Frames)
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
	switch act.Kind {
	case "click":
		mouseOpts, optErr := mouseOptions(act.Button, act.Modifiers)
		if optErr != nil {
			return optErr
		}
		if act.Clicks == 2 {
			mouseOpts = append(mouseOpts, chromedp.ClickCount(2))
		}
		return chromedp.Run(ctx, chromedp.MouseClickNode(node, mouseOpts...))
	case "type":
		return chromedp.Run(ctx, chromedp.KeyEventNode(node, act.Value))
	case "press":
		key, modifiers := browserKey(act.Value)
		if key == "" {
			return fmt.Errorf("browser: key is required")
		}
		return chromedp.Run(ctx, chromedp.KeyEventNode(node, key, browserKeyOptions(act.Value, modifiers)...))
	case "fill":
		return callElementFunction(ctx, node, fmt.Sprintf(`function(){const v=%s;if(!(this instanceof HTMLInputElement||this instanceof HTMLTextAreaElement||this.isContentEditable))throw new Error("element is not fillable");if(this.isContentEditable)this.textContent=v;else{const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set;setter?setter.call(this,v):this.value=v}this.dispatchEvent(new InputEvent("input",{bubbles:true,inputType:"insertText",data:v}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, jsonString(act.Value)))
	case "select_option":
		encoded, _ := json.Marshal(act.Selections)
		return callElementFunction(ctx, node, fmt.Sprintf(`function(){if(!(this instanceof HTMLSelectElement))throw new Error("element is not a select");const specs=%s, multi=this.multiple;for(const o of this.options)o.selected=false;for(const s of specs){let o=null;if(s.value!==undefined)o=[...this.options].find(x=>x.value===s.value);else if(s.label!==undefined)o=[...this.options].find(x=>x.label===s.label);else if(s.index!==undefined)o=this.options[s.index];if(!o)throw new Error("option not found");o.selected=true;if(!multi)break}this.dispatchEvent(new Event("input",{bubbles:true}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, string(encoded)))
	default:
		return fmt.Errorf("browser: unsupported locator action")
	}
}

func (p *cdpPage) ScrollNode(ctx context.Context, ref nodeReference, x, y float64) error {
	root, err := locatorFrameRoot(ctx, ref.Frames)
	if err != nil {
		return err
	}
	var nodes []*cdp.Node
	if err := chromedp.Run(ctx, chromedp.Nodes(ref.Selector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(root))); err != nil || len(nodes) != 1 {
		return fmt.Errorf("browser: node_id is stale")
	}
	return callElementFunction(ctx, nodes[0], fmt.Sprintf(`function(){this.scrollBy({left:%f,top:%f,behavior:"instant"})}`, x, y))
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
