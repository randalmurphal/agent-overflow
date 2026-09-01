package browser

import (
	"encoding/json"
	"fmt"
)

// Shared page JavaScript.
//
// Every element-function body here is used by MORE THAN ONE engine, and that
// is the reason it lives outside any `cdp_*` or `webkit_*` file. The CDP
// driver invokes a body through `Runtime.callFunctionOn` with `this` bound to
// a node it already resolved over the wire; the WebKit driver evaluates the
// same body through `webkitElementCallScript`, which resolves the node in JS
// first and then `.call()`s it. One text, two invocation shapes — so the two
// engines cannot drift on what "fill" or "is visible" means.
//
// A body is always a complete `function(){...}` literal operating on `this`.
// Bodies that would need trusted input (click, type, press) are NOT here:
// CDP dispatches those through the input domain and WebKit through its own
// untrusted tier, so there is nothing shared to factor out.

// locatorResolverJS is the whole locator engine: one function evaluated with
// `this` bound to the document element the locator's frame chain resolved to.
// %s/%s/%d are the JSON locator, the JSON requested attribute, and the match
// cap.
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

// locatorResolverFunction renders locatorResolverJS for one locator.
func locatorResolverFunction(locator Locator, attribute string) string {
	encoded, _ := json.Marshal(locator)
	return fmt.Sprintf(locatorResolverJS, string(encoded), jsonString(attribute), maxLocatorMatches)
}

// nodeReadFunction is the body behind pageDriver.ReadNode. kind is validated
// here rather than in either driver, so an engine cannot quietly accept a
// vocabulary the other rejects.
func nodeReadFunction(kind, argument string) (string, error) {
	switch kind {
	case "attribute":
		return fmt.Sprintf(`function(){const v=this.getAttribute(%s);return v===null?null:v.slice(0,%d)}`, jsonString(argument), maxEvaluateBytes), nil
	case "innerText":
		return fmt.Sprintf(`function(){return String(this.innerText||"").slice(0,%d)}`, maxEvaluateBytes), nil
	case "textContent":
		return fmt.Sprintf(`function(){const v=this.textContent;return v===null?null:String(v).slice(0,%d)}`, maxEvaluateBytes), nil
	case "enabled":
		return `function(){return !(this.disabled||this.getAttribute("aria-disabled")==="true")}`, nil
	case "visible":
		return `function(){const r=this.getBoundingClientRect(),s=getComputedStyle(this);return !!(r.width&&r.height&&s.display!=="none"&&s.visibility!=="hidden"&&s.visibility!=="collapse")}`, nil
	default:
		return "", fmt.Errorf("browser: invalid locator read")
	}
}

// nodeFillFunction is the body behind the `fill` locator action.
func nodeFillFunction(value string) string {
	return fmt.Sprintf(`function(){const v=%s;if(!(this instanceof HTMLInputElement||this instanceof HTMLTextAreaElement||this.isContentEditable))throw new Error("element is not fillable");if(this.isContentEditable)this.textContent=v;else{const setter=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(this),"value")?.set;setter?setter.call(this,v):this.value=v}this.dispatchEvent(new InputEvent("input",{bubbles:true,inputType:"insertText",data:v}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, jsonString(value))
}

// nodeSelectOptionFunction is the body behind the `select_option` action.
func nodeSelectOptionFunction(selections []SelectArg) string {
	encoded, _ := json.Marshal(selections)
	return fmt.Sprintf(`function(){if(!(this instanceof HTMLSelectElement))throw new Error("element is not a select");const specs=%s, multi=this.multiple;for(const o of this.options)o.selected=false;for(const s of specs){let o=null;if(s.value!==undefined)o=[...this.options].find(x=>x.value===s.value);else if(s.label!==undefined)o=[...this.options].find(x=>x.label===s.label);else if(s.index!==undefined)o=this.options[s.index];if(!o)throw new Error("option not found");o.selected=true;if(!multi)break}this.dispatchEvent(new Event("input",{bubbles:true}));this.dispatchEvent(new Event("change",{bubbles:true}));}`, string(encoded))
}

// nodeScrollFunction is the body behind pageDriver.ScrollNode.
func nodeScrollFunction(x, y float64) string {
	return fmt.Sprintf(`function(){this.scrollBy({left:%f,top:%f,behavior:"instant"})}`, x, y)
}
