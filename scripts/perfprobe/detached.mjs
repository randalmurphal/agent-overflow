// Where the node count goes: live census, frame tree, detached trees, documents, scroll surfaces.
// usage: probe detached
import { connectPage, done, evaluate } from './lib/cdp.mjs';
import { SCROLL_SURFACE_SELECTOR } from './lib/dom.mjs';

const c = await connectPage();

console.log('COUNTERS:', JSON.stringify(await c.send('Memory.getDOMCounters')));

console.log('LIVE:', JSON.stringify(await evaluate(c, `(() => {
  let els=0, texts=0, comments=0, other=0, shadowRoots=0, iframeDocs=0;
  const walk = (root) => {
    const w = document.createTreeWalker(root, 0xFFFFFFFF);
    let n = w.currentNode;
    while (n) {
      if (n.nodeType===1) { els++; if (n.shadowRoot) { shadowRoots++; walk(n.shadowRoot); } if (n.tagName==='IFRAME' && n.contentDocument) { iframeDocs++; walk(n.contentDocument); } }
      else if (n.nodeType===3) texts++;
      else if (n.nodeType===8) comments++;
      else other++;
      n = w.nextNode();
    }
  };
  walk(document);
  return { els, texts, comments, other, shadowRoots, iframeDocs, sum: els+texts+comments+other };
})()`)));

const ft = await c.send('Page.getFrameTree');
const frames = [];
(function rec(n) { frames.push({ url: n.frame.url, id: n.frame.id }); (n.childFrames || []).forEach(rec); })(ft.frameTree);
console.log('FRAMES:', JSON.stringify(frames));

// queryObjects enumerates every live Node, which is the only outside view of detached trees.
const proto = await c.send('Runtime.evaluate', { expression: 'Node.prototype' });
const objs = await c.send('Runtime.queryObjects', { prototypeObjectId: proto.result.objectId });
const cnt = await c.send('Runtime.callFunctionOn', {
  objectId: objs.objects.objectId,
  returnByValue: true,
  functionDeclaration: `function() {
    let total = 0, connected = 0, detached = 0, errs = 0;
    let detEls = 0, detTexts = 0;
    const roots = new Map(); const tags = new Map();
    for (let i = 0; i < this.length; i++) {
      const n = this[i]; total++;
      let ok = false, conn = false;
      try { conn = n.isConnected; ok = true; } catch (e) { errs++; }
      if (!ok) continue;
      if (conn) { connected++; continue; }
      detached++;
      try {
        const t = n.nodeType;
        if (t === 1) { detEls++; tags.set(n.tagName, (tags.get(n.tagName)||0)+1); }
        else if (t === 3) detTexts++;
        let top = n, hops = 0;
        while (top.parentNode && hops++ < 200) top = top.parentNode;
        const cls = top.nodeType===1 ? (''+(top.className||'')).slice(0,60) : '';
        const key = top.nodeName + (cls?('.'+cls):'');
        roots.set(key, (roots.get(key)||0)+1);
      } catch (e) { errs++; }
    }
    return JSON.stringify({
      total, connected, detached, detEls, detTexts, errs,
      topRoots: [...roots.entries()].sort((a,b)=>b[1]-a[1]).slice(0,25),
      topTags: [...tags.entries()].sort((a,b)=>b[1]-a[1]).slice(0,15),
    });
  }`,
});
console.log('DETACHED:', cnt.result?.value ?? JSON.stringify(cnt.exceptionDetails || {}).slice(0, 400));

const dproto = await c.send('Runtime.evaluate', { expression: 'Document.prototype' });
const dobjs = await c.send('Runtime.queryObjects', { prototypeObjectId: dproto.result.objectId });
const dcnt = await c.send('Runtime.callFunctionOn', {
  objectId: dobjs.objects.objectId,
  returnByValue: true,
  functionDeclaration: `function() {
    const out = [];
    for (let i = 0; i < this.length; i++) {
      const d = this[i];
      try { out.push({ url: (d.URL||'').slice(0,80), nodes: d.getElementsByTagName('*').length, ctor: d.constructor.name }); } catch(e) { out.push({err:1}); }
    }
    return JSON.stringify(out);
  }`,
});
console.log('DOCS:', dcnt.result?.value ?? 'ERR');

console.log('SCROLL SURFACES:', JSON.stringify(await evaluate(c, `(() => {
  return [...document.querySelectorAll(${JSON.stringify(SCROLL_SURFACE_SELECTOR)})].map(el => {
    const r = el.getBoundingClientRect();
    const chain = [];
    let p = el;
    for (let i=0;i<8 && p;i++,p=p.parentElement) chain.push(p.tagName + (p.className?'.'+(''+p.className).split(' ').slice(0,3).join('.'):''));
    let hid = null, q = el;
    while (q) { const cs = getComputedStyle(q); if (cs.display==='none'||cs.visibility==='hidden'||cs.contentVisibility==='hidden') { hid = q.tagName+'.'+(''+q.className).slice(0,50)+':'+cs.display+'/'+cs.visibility+'/'+cs.contentVisibility; break;} q=q.parentElement; }
    return { rect: {x:Math.round(r.x),y:Math.round(r.y),w:Math.round(r.width),h:Math.round(r.height)}, hidden: hid, chain: chain.slice(0,5) };
  });
})()`), null, 1));
await done([c]);
