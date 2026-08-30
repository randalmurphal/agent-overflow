package browser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpio "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

const (
	maxAssetInventory      = 2000
	maxInlineSVGs          = 100
	maxInlineSVGBytes      = 32 << 10
	maxAssetBytes          = 128 << 20
	maxAssetBundleBytes    = 512 << 20
	maxAssetURLBytes       = 16 << 10
	maxAssetNameBytes      = 512
	maxAssetInventoryBytes = 8 << 20
	maxAssetSources        = 20
	maxAssetScanElements   = maxNodeReferences
)

func (s *AssetSource) UnmarshalJSON(data []byte) error {
	var wire struct {
		Kind     string `json:"kind"`
		NodeID   string `json:"nodeId"`
		Property string `json:"property"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	s.Kind, s.NodeID, s.Property, s.selector = wire.Kind, wire.NodeID, wire.Property, wire.Selector
	return nil
}

func (m *Manager) Assets(ctx context.Context, access Access, opts AssetOptions) (any, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "list":
		return m.listAssets(ctx, p)
	case "bundle":
		return m.bundleAssets(ctx, p, opts)
	default:
		return nil, fmt.Errorf("browser: asset action must be list or bundle")
	}
}

func (m *Manager) listAssets(ctx context.Context, p *managedPage) (AssetInventory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	var raw struct {
		PageURL    string      `json:"pageUrl"`
		Assets     []AssetInfo `json:"assets"`
		InlineSVGs []InlineSVG `json:"inlineSvgs"`
	}
	if err := chromedp.Run(opCtx, chromedp.Evaluate(assetInventoryExpression(), &raw)); err != nil {
		return AssetInventory{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	seen := make(map[string]int, len(raw.Assets))
	nodeIDs := make(map[string]string)
	assets := make([]AssetInfo, 0, len(raw.Assets))
	summary := make(map[string]int)
	for _, asset := range raw.Assets {
		if asset.URL == "" {
			continue
		}
		key := asset.Kind + "\x00" + asset.URL
		index, exists := seen[key]
		if !exists && len(assets) >= maxAssetInventory {
			continue
		}
		for i := range asset.Sources {
			selector := asset.Sources[i].selector
			asset.Sources[i].selector = ""
			if selector != "" {
				id := nodeIDs[selector]
				if id == "" && len(nodeIDs) < maxNodeReferences {
					id = p.rememberNode(nodeReference{Selector: selector})
					nodeIDs[selector] = id
				}
				asset.Sources[i].NodeID = id
			}
		}
		if exists {
			assets[index].Sources = mergeAssetSources(assets[index].Sources, asset.Sources)
			continue
		}
		digest := sha256.Sum256([]byte(key))
		asset.ID = fmt.Sprintf("%x", digest[:12])
		asset.Name = safeArtifactName(asset.Name, asset.Kind)
		asset.Sources = mergeAssetSources(nil, asset.Sources)
		seen[key] = len(assets)
		assets = append(assets, asset)
		summary[asset.Kind]++
	}
	sort.Slice(assets, func(i, j int) bool {
		if assets[i].Kind == assets[j].Kind {
			return assets[i].URL < assets[j].URL
		}
		return assets[i].Kind < assets[j].Kind
	})
	for i := range raw.InlineSVGs {
		raw.InlineSVGs[i].Name = truncateUTF8(raw.InlineSVGs[i].Name, maxAssetNameBytes)
		raw.InlineSVGs[i].Markup = truncateUTF8(raw.InlineSVGs[i].Markup, maxInlineSVGBytes)
	}
	raw.PageURL = truncateUTF8(raw.PageURL, maxBrowserURLBytes)
	inventory := AssetInventory{ID: uuid.NewString(), PageURL: raw.PageURL, Assets: assets, InlineSVGs: raw.InlineSVGs, Summary: AssetSummary{ByKind: summary, TotalCount: len(assets), InlineSVGCount: len(raw.InlineSVGs)}}
	if encoded, encodeErr := json.Marshal(inventory); encodeErr != nil || len(encoded) > maxAssetInventoryBytes {
		return AssetInventory{}, fmt.Errorf("browser: asset inventory exceeds %d bytes", maxAssetInventoryBytes)
	}
	p.assetMu.Lock()
	if len(p.inventories) >= 5 {
		delete(p.inventories, p.assetOrder[0])
		p.assetOrder = p.assetOrder[1:]
	}
	p.inventories[inventory.ID] = inventory
	p.assetOrder = append(p.assetOrder, inventory.ID)
	p.assetMu.Unlock()
	return inventory, nil
}

func mergeAssetSources(existing, additions []AssetSource) []AssetSource {
	for _, addition := range additions {
		if len(existing) >= maxAssetSources {
			break
		}
		duplicate := false
		for _, current := range existing {
			if current.Kind == addition.Kind && current.NodeID == addition.NodeID && current.Property == addition.Property {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, addition)
		}
	}
	return existing
}

func (m *Manager) bundleAssets(ctx context.Context, p *managedPage, opts AssetOptions) (AssetBundle, error) {
	if len(opts.AssetIDs) > 200 || len(opts.Kinds) > 4 {
		return AssetBundle{}, fmt.Errorf("browser: asset bundle selection exceeds its limit")
	}
	p.assetMu.Lock()
	inventory, ok := p.inventories[strings.TrimSpace(opts.InventoryID)]
	p.assetMu.Unlock()
	if !ok {
		return AssetBundle{}, fmt.Errorf("browser: asset inventory not found or expired")
	}
	ids := make(map[string]bool, len(opts.AssetIDs))
	for _, id := range opts.AssetIDs {
		ids[id] = true
	}
	kinds := make(map[string]bool, len(opts.Kinds))
	for _, kind := range opts.Kinds {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind != "font" && kind != "image" && kind != "stylesheet" && kind != "video" {
			return AssetBundle{}, fmt.Errorf("browser: unsupported bundle kind %q", kind)
		}
		kinds[kind] = true
	}
	if len(ids) == 0 && len(kinds) == 0 {
		return AssetBundle{}, fmt.Errorf("browser: bundle requires asset_ids or kinds")
	}
	available := make(map[string]bool, len(inventory.Assets))
	for _, asset := range inventory.Assets {
		available[asset.ID] = true
	}
	for id := range ids {
		if !available[id] {
			return AssetBundle{}, fmt.Errorf("browser: asset id %q is not in inventory", id)
		}
	}
	selected := make([]AssetInfo, 0)
	for _, asset := range inventory.Assets {
		if len(ids) > 0 && !ids[asset.ID] {
			continue
		}
		if len(kinds) > 0 && !kinds[asset.Kind] {
			continue
		}
		if len(ids) == 0 && len(kinds) == 0 {
			continue
		}
		selected = append(selected, asset)
	}
	if len(selected) > 200 {
		return AssetBundle{}, fmt.Errorf("browser: asset bundle exceeds 200 files")
	}
	if !m.reserveArtifacts(maxAssetBundleBytes) {
		return AssetBundle{}, fmt.Errorf("browser: artifacts exceed %d bytes", maxArtifactRootBytes)
	}
	var settledBytes int64
	defer func() { m.settleArtifactReservation(maxAssetBundleBytes, settledBytes) }()
	dir := filepath.Join(m.artifactRoot, "assets", uuid.NewString())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return AssetBundle{}, err
	}
	bundle := AssetBundle{DirectoryPath: dir, Assets: make([]BundledAsset, 0, len(selected)), Failures: make([]AssetFailure, 0)}
	started := time.Now()
	var total int64
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	frameTree, err := page.GetFrameTree().Do(targetCommandContext(opCtx))
	if err != nil {
		return AssetBundle{}, err
	}
	for _, asset := range selected {
		if !m.navigationAllowed(p.access, asset.URL) {
			bundle.Failures = append(bundle.Failures, AssetFailure{ID: asset.ID, Name: asset.Name, URL: asset.URL, Reason: "asset URL is outside browser navigation authority"})
			continue
		}
		result, loadErr := network.LoadNetworkResource(asset.URL, &network.LoadNetworkResourceOptions{DisableCache: false, IncludeCredentials: true}).WithFrameID(frameTree.Frame.ID).Do(targetCommandContext(opCtx))
		if loadErr != nil || result == nil || !result.Success || result.Stream == "" {
			reason := "load failed"
			if loadErr != nil {
				reason = loadErr.Error()
			} else if result != nil && result.NetErrorName != "" {
				reason = result.NetErrorName
			}
			bundle.Failures = append(bundle.Failures, AssetFailure{ID: asset.ID, Name: asset.Name, URL: asset.URL, Reason: reason})
			continue
		}
		contentType := ""
		for key, value := range result.Headers {
			if strings.EqualFold(key, "content-type") {
				contentType = fmt.Sprint(value)
				break
			}
		}
		name := safeArtifactName(asset.Name, asset.Kind)
		path, pathErr := uniqueArtifactPath(dir, name)
		if pathErr != nil {
			bundle.Failures = append(bundle.Failures, AssetFailure{ID: asset.ID, Name: name, URL: asset.URL, Reason: pathErr.Error(), ContentType: contentType})
			continue
		}
		file, fileErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if fileErr != nil {
			bundle.Failures = append(bundle.Failures, AssetFailure{ID: asset.ID, Name: name, URL: asset.URL, Reason: fileErr.Error(), ContentType: contentType})
			continue
		}
		written, copyErr := readCDPStream(opCtx, result.Stream, file, maxAssetBytes, min64(maxAssetBundleBytes-total, maxAssetBytes))
		closeErr := file.Close()
		_ = cdpio.Close(result.Stream).Do(targetCommandContext(opCtx))
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			_ = os.Remove(path)
			bundle.Failures = append(bundle.Failures, AssetFailure{ID: asset.ID, Name: name, URL: asset.URL, Reason: copyErr.Error(), ContentType: contentType})
			continue
		}
		total += written
		settledBytes += written
		bundle.Assets = append(bundle.Assets, BundledAsset{AssetInfo: asset, Path: path, ContentType: contentType})
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	manifest, _ := json.MarshalIndent(map[string]any{"inventoryId": inventory.ID, "pageUrl": inventory.PageURL, "assets": bundle.Assets, "failures": bundle.Failures}, "", "  ")
	if err := os.WriteFile(manifestPath, append(manifest, '\n'), 0o600); err != nil {
		return AssetBundle{}, err
	}
	settledBytes += int64(len(manifest) + 1)
	bundle.ManifestPath = manifestPath
	bundle.Summary = map[string]any{"requestedCount": len(selected), "downloadedCount": len(bundle.Assets), "failedCount": len(bundle.Failures), "elapsedMs": time.Since(started).Milliseconds(), "bytes": total}
	return bundle, nil
}

func readCDPStream(ctx context.Context, handle cdpio.StreamHandle, file *os.File, perFile, remaining int64) (int64, error) {
	var written int64
	for {
		var read cdpio.ReadReturns
		err := cdp.Execute(targetCommandContext(ctx), cdpio.CommandRead, cdpio.Read(handle).WithSize(1<<20), &read)
		if err != nil {
			return written, err
		}
		chunk := []byte(read.Data)
		if read.Base64encoded {
			chunk, err = base64.StdEncoding.DecodeString(read.Data)
			if err != nil {
				return written, err
			}
		}
		if int64(len(chunk))+written > perFile || int64(len(chunk))+written > remaining {
			return written, fmt.Errorf("browser: asset exceeds bundle size limit")
		}
		n, err := file.Write(chunk)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if read.EOF {
			return written, nil
		}
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func assetInventoryExpression() string {
	return fmt.Sprintf(`(()=>{
const assets=[],assetCap=%d,resourceCap=%d,elementCap=%d;let budget=%d;
const path=el=>{if(el.id&&el.id.length<=256)return "#"+CSS.escape(el.id);const parts=[];for(let n=el;n&&n.nodeType===1&&parts.length<12;n=n.parentElement){let s=n.tagName.toLowerCase();if(n.parentElement){const same=[...n.parentElement.children].filter(x=>x.tagName===n.tagName);if(same.length>1)s+=":nth-of-type("+(same.indexOf(n)+1)+")"}parts.unshift(s)}return parts.join(">").slice(0,4096)};
const push=(url,kind,name,source)=>{if(assets.length>=assetCap||budget<=0)return;try{url=new URL(url,location.href).href}catch{return};if(url.length>%d)return;const assetName=String(name||url.split("/").pop()||kind).slice(0,%d),cost=4*(url.length+assetName.length+JSON.stringify(source).length)+64;if(cost>budget)return;budget-=cost;assets.push({id:"",url,kind,name:assetName,sources:[source]})};
for(const e of performance.getEntriesByType("resource").slice(0,resourceCap)){const t=e.initiatorType;push(e.name,t==="img"?"image":t==="css"?"stylesheet":t==="script"?"script":t==="video"?"video":t==="link"&&/\.(woff2?|ttf|otf)(\?|$)/i.test(e.name)?"font":"other","",{kind:"resource"})}
const attributed=document.querySelectorAll("img[src],source[src],video[src],audio[src],link[href],script[src]");
for(let i=0;i<Math.min(attributed.length,elementCap)&&assets.length<assetCap;i++){const el=attributed[i],attr=el.hasAttribute("href")?"href":"src",url=el.getAttribute(attr),tag=el.tagName.toLowerCase(),rel=el.getAttribute("rel")||"",as=el.getAttribute("as")||"",kind=tag==="img"||tag==="source"||as==="image"?"image":tag==="video"||tag==="audio"?"video":tag==="link"&&rel.includes("stylesheet")?"stylesheet":tag==="link"&&(rel.includes("font")||as==="font"||/\.(woff2?|ttf|otf)(\?|$)/i.test(url||""))?"font":tag==="script"?"script":"other";push(url,kind,el.getAttribute("download")||el.getAttribute("alt")||"",{kind:"attribute",property:attr,selector:path(el)})}
const styled=document.querySelectorAll("body *"),props=["backgroundImage","borderImageSource","listStyleImage","maskImage","webkitMaskImage","cursor","content"],urls=/url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*?))\s*\)/g;
for(let i=0;i<Math.min(styled.length,elementCap)&&assets.length<assetCap;i++){const el=styled[i],style=getComputedStyle(el),selector=path(el);for(const property of props){const value=style[property]||"";urls.lastIndex=0;for(let match;(match=urls.exec(value))&&assets.length<assetCap;)push(match[1]||match[2]||match[3],"image","",{kind:"computedStyle",property,selector})}}
const svgNodes=document.querySelectorAll("svg"),svgs=[];for(let i=0;i<Math.min(svgNodes.length,%d);i++){const svg=svgNodes[i];svgs.push({id:"svg-"+i,name:String(svg.getAttribute("aria-label")||svg.id||"inline-svg-"+(i+1)).slice(0,%d),markup:svg.outerHTML.slice(0,%d)})}
return {pageUrl:location.href.slice(0,%d),assets,inlineSvgs:svgs}})()`, maxAssetInventory*4, maxAssetInventory, maxAssetScanElements, maxAssetInventoryBytes/2, maxAssetURLBytes/4, maxAssetNameBytes/4, maxInlineSVGs, maxAssetNameBytes/4, maxInlineSVGBytes/4, maxBrowserURLBytes/4)
}
