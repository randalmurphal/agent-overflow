package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func (p *cdpPage) Click(ctx context.Context, selector string) error {
	return chromedp.Do(ctx, chromedp.ScrollIntoView(chromedp.CSS(selector)), chromedp.Click(chromedp.CSS(selector)))
}

func (p *cdpPage) Type(ctx context.Context, selector, text string, clear bool) error {
	steps := []chromedp.Action[chromedp.Void]{chromedp.Focus(chromedp.CSS(selector))}
	if clear {
		steps = append(steps, chromedp.KeyEvent("a", browserKeyOptions("ControlOrMeta+a", controlOrMetaModifier())...), chromedp.KeyEvent(kb.Backspace))
	}
	steps = append(steps, chromedp.SendKeys(chromedp.CSS(selector), text))
	return chromedp.Do(ctx, steps...)
}

func (p *cdpPage) Press(ctx context.Context, raw string) error {
	key, modifiers := browserKey(raw)
	if key == "" {
		return fmt.Errorf("browser: key is required")
	}
	if err := chromedp.Do(ctx, chromedp.KeyEvent(key, browserKeyOptions(raw, modifiers)...)); err != nil {
		return fmt.Errorf("browser: press key: %w", err)
	}
	return nil
}

func (p *cdpPage) TypeText(ctx context.Context, text string) error {
	return chromedp.Do(ctx, chromedp.KeyEvent(text))
}

func (p *cdpPage) SelectionText(ctx context.Context) string {
	selected, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`(()=>{const a=document.activeElement;if(a&&(a instanceof HTMLInputElement||a instanceof HTMLTextAreaElement)&&a.selectionStart!==null)return a.value.slice(a.selectionStart,a.selectionEnd);return String(getSelection()||"")})()`))
	return selected
}

func (p *cdpPage) Scroll(ctx context.Context, selector string, x, y float64) error {
	selectorJSON, _ := json.Marshal(selector)
	expression := fmt.Sprintf(`(() => { const s=%s; const el=s?document.querySelector(s):window; if(!el) throw new Error("selector not found"); el.scrollBy({left:%f,top:%f,behavior:"instant"}); return true; })()`, selectorJSON, x, y)
	_, err := chromedp.Run(ctx, chromedp.Evaluate[bool](expression))
	return err
}

func (p *cdpPage) WaitVisible(ctx context.Context, selector string) error {
	return chromedp.Do(ctx, chromedp.WaitVisible(chromedp.CSS(selector)))
}

func (p *cdpPage) Pointer(ctx context.Context, opts PointerOptions) error {
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	modifiers, err := inputModifiers(opts.Modifiers)
	if err != nil {
		return err
	}
	modifierMask := chromedp.ModifierNone
	for _, modifier := range modifiers {
		modifierMask |= modifier
	}
	mask := int64(modifierMask)
	button, err := inputButton(opts.Button)
	if err != nil {
		return err
	}
	mouse := func(params input.DispatchMouseEventParams) error {
		params.Modifiers = mask
		_, err := chromedp.Call(ctx, input.DispatchMouseEvent, params)
		return err
	}
	switch action {
	case "click", "double_click":
		count := int64(1)
		if action == "double_click" {
			count = 2
		}
		if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MousePressed, X: opts.X, Y: opts.Y, Button: button, ClickCount: count}); err != nil {
			return err
		}
		if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MouseReleased, X: opts.X, Y: opts.Y, Button: button, ClickCount: count}); err != nil {
			return err
		}
	case "move":
		if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MouseMoved, X: opts.X, Y: opts.Y, Button: input.MouseButtonNone}); err != nil {
			return err
		}
	case "scroll":
		if err := validateScrollDelta(opts.ScrollX, opts.ScrollY); err != nil {
			return err
		}
		if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MouseWheel, X: opts.X, Y: opts.Y, DeltaX: opts.ScrollX, DeltaY: opts.ScrollY}); err != nil {
			return err
		}
	case "drag":
		if len(opts.Path) < 2 {
			return fmt.Errorf("browser: drag path requires at least two points")
		}
		for _, point := range opts.Path {
			if err := validatePoint(point); err != nil {
				return err
			}
		}
		return p.drag(ctx, opts.Path, mask, mouse)
	default:
		return fmt.Errorf("browser: pointer action must be click, double_click, move, scroll, or drag")
	}
	return nil
}

// drag presses at the first point, moves through the rest and releases at
// the last. When the page starts a native drag, Chrome intercepts it and
// reports its data, which the drag events replay.
func (p *cdpPage) drag(ctx context.Context, path []Point, mask int64, mouse func(input.DispatchMouseEventParams) error) error {
	first, last := path[0], path[len(path)-1]
	// The subscription ends with the drag, so an interception reported for
	// another drag is never read as this one's.
	dragCtx, endDrag := context.WithCancel(ctx)
	defer endDrag()
	dragData := make(chan *input.DragData, 1)
	onTargetEvent(dragCtx, input.DragIntercepted, func(string, ...any) {}, func(intercepted input.EventDragIntercepted) {
		if intercepted.Data != nil {
			select {
			case dragData <- intercepted.Data:
			default:
			}
		}
	})
	if _, err := chromedp.Call(ctx, input.SetInterceptDrags, input.SetInterceptDragsParams{Enabled: true}); err != nil {
		return err
	}
	defer func() {
		_, _ = chromedp.Call(ctx, input.SetInterceptDrags, input.SetInterceptDragsParams{Enabled: false})
	}()
	if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MouseMoved, X: first.X, Y: first.Y, Button: input.MouseButtonNone}); err != nil {
		return err
	}
	if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MousePressed, X: first.X, Y: first.Y, Button: input.MouseButtonLeft, Buttons: 1, ClickCount: 1}); err != nil {
		return err
	}
	for _, point := range path[1:] {
		if err := mouse(input.DispatchMouseEventParams{Type: chromedp.MouseMoved, X: point.X, Y: point.Y, Button: input.MouseButtonNone, Buttons: 1}); err != nil {
			return err
		}
	}
	var intercepted *input.DragData
	select {
	case intercepted = <-dragData:
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	if intercepted == nil {
		intercepted = &input.DragData{Items: []*input.DragDataItem{}, DragOperationsMask: 1}
	}
	for i, point := range path[1:] {
		kind := input.DispatchDragEventTypeDragOver
		if i == 0 {
			kind = input.DispatchDragEventTypeDragEnter
		}
		if _, err := chromedp.Call(ctx, input.DispatchDragEvent, input.DispatchDragEventParams{Type: kind, X: point.X, Y: point.Y, Data: intercepted, Modifiers: mask}); err != nil {
			return err
		}
	}
	if _, err := chromedp.Call(ctx, input.DispatchDragEvent, input.DispatchDragEventParams{Type: input.DispatchDragEventTypeDrop, X: last.X, Y: last.Y, Data: intercepted, Modifiers: mask}); err != nil {
		return err
	}
	return mouse(input.DispatchMouseEventParams{Type: chromedp.MouseReleased, X: last.X, Y: last.Y, Button: input.MouseButtonLeft, Buttons: 0, ClickCount: 1})
}

func inputButton(raw string) (input.MouseButton, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "left":
		return input.MouseButtonLeft, nil
	case "right":
		return input.MouseButtonRight, nil
	case "middle":
		return input.MouseButtonMiddle, nil
	case "back":
		return input.MouseButtonBack, nil
	case "forward":
		return input.MouseButtonForward, nil
	default:
		return input.MouseButtonNone, fmt.Errorf("browser: button must be left, right, middle, back, or forward")
	}
}

func inputModifiers(raw []string) ([]chromedp.Modifier, error) {
	out := make([]chromedp.Modifier, 0, len(raw))
	for _, value := range raw {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "alt":
			out = append(out, chromedp.ModifierAlt)
		case "control", "ctrl":
			out = append(out, chromedp.ModifierCtrl)
		case "meta", "command", "cmd":
			out = append(out, chromedp.ModifierMeta)
		case "shift":
			out = append(out, chromedp.ModifierShift)
		case "controlormeta":
			out = append(out, controlOrMetaModifier())
		default:
			return nil, fmt.Errorf("browser: unsupported modifier %q", value)
		}
	}
	return out, nil
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
	mods, err := inputModifiers(modifiers)
	if err != nil {
		return nil, err
	}
	if len(mods) > 0 {
		opts = append(opts, chromedp.ButtonModifiers(mods...))
	}
	return opts, nil
}

func browserEditingCommand(command string) chromedp.KeyOption {
	return func(event *input.DispatchKeyEventParams) {
		if event.Type == kb.KeyDown || event.Type == kb.KeyRawDown {
			event.Commands = []string{command}
		}
	}
}

func browserKeyOptions(raw string, modifiers chromedp.Modifier) []chromedp.KeyOption {
	options := []chromedp.KeyOption{chromedp.KeyModifiers(modifiers)}
	if isModifierChord(raw, "a") {
		options = append(options, browserEditingCommand("selectAll"))
	}
	return options
}

func browserKey(raw string) (string, chromedp.Modifier) {
	parts := strings.Split(strings.TrimSpace(raw), "+")
	if len(parts) == 0 {
		return "", 0
	}
	out := ""
	var modifiers chromedp.Modifier
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "control", "ctrl":
			modifiers |= chromedp.ModifierCtrl
		case "shift":
			modifiers |= chromedp.ModifierShift
		case "alt", "option":
			modifiers |= chromedp.ModifierAlt
		case "meta", "command", "cmd":
			modifiers |= chromedp.ModifierMeta
		case "controlormeta":
			modifiers |= controlOrMetaModifier()
		case "enter", "return":
			out += kb.Enter
		case "tab":
			out += kb.Tab
		case "escape", "esc":
			out += kb.Escape
		case "backspace":
			out += kb.Backspace
		case "delete":
			out += kb.Delete
		case "arrowup", "up":
			out += kb.ArrowUp
		case "arrowdown", "down":
			out += kb.ArrowDown
		case "arrowleft", "left":
			out += kb.ArrowLeft
		case "arrowright", "right":
			out += kb.ArrowRight
		case "space":
			out += " "
		default:
			out += part
		}
	}
	return out, modifiers
}

func controlOrMetaModifier() chromedp.Modifier {
	if runtime.GOOS == "darwin" {
		return chromedp.ModifierMeta
	}
	return chromedp.ModifierCtrl
}
