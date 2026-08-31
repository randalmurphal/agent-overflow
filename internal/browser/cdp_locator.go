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
	fn := locatorResolverFunction(locator, attribute)
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
	fn, err := nodeReadFunction(kind, argument)
	if err != nil {
		return nil, err
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
		return callElementFunction(ctx, node, nodeFillFunction(act.Value))
	case "select_option":
		return callElementFunction(ctx, node, nodeSelectOptionFunction(act.Selections))
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
	return callElementFunction(ctx, nodes[0], nodeScrollFunction(x, y))
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
