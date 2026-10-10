package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	obj, err := resolveNodeObject(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("resolve frame root: %w", err)
	}
	defer releaseObject(ctx, obj)
	result, err := chromedp.Call(ctx, cdpruntime.CallFunctionOn, cdpruntime.CallFunctionOnParams{
		FunctionDeclaration: fn, ObjectID: obj.ObjectID, ReturnByValue: new(true), AwaitPromise: new(true),
	})
	if err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil {
		return nil, fmt.Errorf("%s", result.ExceptionDetails.Text)
	}
	remote := result.Result
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
	nodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(match.Selector), chromedp.AtLeast(0), chromedp.FromNode(root)))
	if err != nil || len(nodes) != 1 {
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
	nodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(match.Selector), chromedp.AtLeast(0), chromedp.FromNode(root)))
	if err != nil {
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
		return chromedp.Do(ctx, chromedp.MouseClickNode(node, mouseOpts...))
	case "type":
		return chromedp.Do(ctx, chromedp.KeyEventNode(node, act.Value))
	case "press":
		key, modifiers := browserKey(act.Value)
		if key == "" {
			return fmt.Errorf("browser: key is required")
		}
		return chromedp.Do(ctx, chromedp.KeyEventNode(node, key, browserKeyOptions(act.Value, modifiers)...))
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
	nodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(ref.Selector), chromedp.AtLeast(0), chromedp.FromNode(root)))
	if err != nil || len(nodes) != 1 {
		return fmt.Errorf("browser: node_id is stale")
	}
	return callElementFunction(ctx, nodes[0], nodeScrollFunction(x, y))
}

func locatorFrameRoot(ctx context.Context, frames []string) (*chromedp.Node, error) {
	roots, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll("html"), chromedp.AtLeast(0)))
	if err != nil || len(roots) != 1 {
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
		frameNodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(selector), chromedp.AtLeast(0), chromedp.FromNode(root)))
		if err != nil {
			return nil, err
		}
		if len(frameNodes) != 1 {
			return nil, fmt.Errorf("browser: frame selector %q resolved to %d elements", selector, len(frameNodes))
		}
		frameRoots, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll("html"), chromedp.AtLeast(0), chromedp.FromNode(frameNodes[0])))
		if err != nil {
			return nil, err
		}
		if len(frameRoots) != 1 {
			return nil, fmt.Errorf("browser: frame %q is not ready or accessible", selector)
		}
		root = frameRoots[0]
	}
	return root, nil
}

func callElementFunction(ctx context.Context, node *chromedp.Node, fn string) error {
	_, err := callElementFunctionValue(ctx, node, fn)
	return err
}

func callElementFunctionValue(ctx context.Context, node *chromedp.Node, fn string) (any, error) {
	obj, err := resolveNodeObject(ctx, node)
	if err != nil {
		return nil, err
	}
	defer releaseObject(ctx, obj)
	result, err := chromedp.Call(ctx, cdpruntime.CallFunctionOn, cdpruntime.CallFunctionOnParams{
		FunctionDeclaration: fn, ObjectID: obj.ObjectID, ReturnByValue: new(true), UserGesture: new(true),
	})
	if err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil {
		return nil, fmt.Errorf("browser: element action: %s", result.ExceptionDetails.Text)
	}
	remote := result.Result
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

// resolveNodeObject resolves a node to the JavaScript object a function is
// called on.
func resolveNodeObject(ctx context.Context, node *chromedp.Node) (*cdpruntime.RemoteObject, error) {
	resolved, err := chromedp.Call(ctx, dom.ResolveNode, dom.ResolveNodeParams{NodeID: node.NodeID})
	if err != nil {
		return nil, err
	}
	if resolved.Object == nil {
		return nil, errors.New("the engine resolved the node to no object")
	}
	return resolved.Object, nil
}

// releaseObject frees an object resolveNodeObject made. The page frees it
// with its document anyway, so a failure costs only memory until then.
func releaseObject(ctx context.Context, obj *cdpruntime.RemoteObject) {
	_, _ = chromedp.Call(ctx, cdpruntime.ReleaseObject, cdpruntime.ReleaseObjectParams{ObjectID: obj.ObjectID})
}
