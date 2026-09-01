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
	return chromedp.Run(ctx, chromedp.ScrollIntoView(selector, chromedp.ByQuery), chromedp.Click(selector, chromedp.ByQuery))
}

func (p *cdpPage) Type(ctx context.Context, selector, text string, clear bool) error {
	actions := []chromedp.Action{chromedp.Focus(selector, chromedp.ByQuery)}
	if clear {
		actions = append(actions, chromedp.KeyEvent("a", browserKeyOptions("ControlOrMeta+a", controlOrMetaModifier())...), chromedp.KeyEvent(kb.Backspace))
	}
	actions = append(actions, chromedp.SendKeys(selector, text, chromedp.ByQuery))
	return chromedp.Run(ctx, actions...)
}

func (p *cdpPage) Press(ctx context.Context, raw string) error {
	key, modifiers := browserKey(raw)
	if key == "" {
		return fmt.Errorf("browser: key is required")
	}
	if err := chromedp.Run(ctx, chromedp.KeyEvent(key, browserKeyOptions(raw, modifiers)...)); err != nil {
		return fmt.Errorf("browser: press key: %w", err)
	}
	return nil
}

func (p *cdpPage) TypeText(ctx context.Context, text string) error {
	return chromedp.Run(ctx, chromedp.KeyEvent(text))
}

func (p *cdpPage) SelectionText(ctx context.Context) string {
	var selected string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{const a=document.activeElement;if(a&&(a instanceof HTMLInputElement||a instanceof HTMLTextAreaElement)&&a.selectionStart!==null)return a.value.slice(a.selectionStart,a.selectionEnd);return String(getSelection()||"")})()`, &selected))
	return selected
}

func (p *cdpPage) Scroll(ctx context.Context, selector string, x, y float64) error {
	selectorJSON, _ := json.Marshal(selector)
	expression := fmt.Sprintf(`(() => { const s=%s; const el=s?document.querySelector(s):window; if(!el) throw new Error("selector not found"); el.scrollBy({left:%f,top:%f,behavior:"instant"}); return true; })()`, selectorJSON, x, y)
	var ok bool
	return chromedp.Run(ctx, chromedp.Evaluate(expression, &ok))
}

func (p *cdpPage) WaitVisible(ctx context.Context, selector string) error {
	return chromedp.Run(ctx, chromedp.WaitVisible(selector, chromedp.ByQuery))
}

func (p *cdpPage) Pointer(ctx context.Context, opts PointerOptions) error {
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	modifiers, err := inputModifiers(opts.Modifiers)
	if err != nil {
		return err
	}
	modifierMask := input.ModifierNone
	for _, modifier := range modifiers {
		modifierMask |= modifier
	}
	button, err := inputButton(opts.Button)
	if err != nil {
		return err
	}
	targetCtx := targetCommandContext(ctx)
	switch action {
	case "click", "double_click":
		count := int64(1)
		if action == "double_click" {
			count = 2
		}
		if err := input.DispatchMouseEvent(input.MousePressed, opts.X, opts.Y).WithButton(button).WithModifiers(modifierMask).WithClickCount(count).Do(targetCtx); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MouseReleased, opts.X, opts.Y).WithButton(button).WithModifiers(modifierMask).WithClickCount(count).Do(targetCtx); err != nil {
			return err
		}
	case "move":
		if err := input.DispatchMouseEvent(input.MouseMoved, opts.X, opts.Y).WithButton(input.None).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return err
		}
	case "scroll":
		if err := validateScrollDelta(opts.ScrollX, opts.ScrollY); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MouseWheel, opts.X, opts.Y).WithDeltaX(opts.ScrollX).WithDeltaY(opts.ScrollY).WithModifiers(modifierMask).Do(targetCtx); err != nil {
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
		first := opts.Path[0]
		dragData := make(chan *input.DragData, 1)
		chromedp.ListenTarget(ctx, func(event any) {
			if intercepted, ok := event.(*input.EventDragIntercepted); ok && intercepted.Data != nil {
				select {
				case dragData <- intercepted.Data:
				default:
				}
			}
		})
		if err := input.SetInterceptDrags(true).Do(targetCtx); err != nil {
			return err
		}
		defer func() { _ = input.SetInterceptDrags(false).Do(targetCtx) }()
		if err := input.DispatchMouseEvent(input.MouseMoved, first.X, first.Y).WithButton(input.None).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MousePressed, first.X, first.Y).WithButton(input.Left).WithButtons(1).WithModifiers(modifierMask).WithClickCount(1).Do(targetCtx); err != nil {
			return err
		}
		for _, point := range opts.Path[1:] {
			if err := input.DispatchMouseEvent(input.MouseMoved, point.X, point.Y).WithButton(input.None).WithButtons(1).WithModifiers(modifierMask).Do(targetCtx); err != nil {
				return err
			}
		}
		last := opts.Path[len(opts.Path)-1]
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
		for i, point := range opts.Path[1:] {
			kind := input.DragOver
			if i == 0 {
				kind = input.DragEnter
			}
			if err := input.DispatchDragEvent(kind, point.X, point.Y, intercepted).WithModifiers(modifierMask).Do(targetCtx); err != nil {
				return err
			}
		}
		if err := input.DispatchDragEvent(input.Drop, last.X, last.Y, intercepted).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MouseReleased, last.X, last.Y).WithButton(input.Left).WithButtons(0).WithModifiers(modifierMask).WithClickCount(1).Do(targetCtx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("browser: pointer action must be click, double_click, move, scroll, or drag")
	}
	return nil
}

func inputButton(raw string) (input.MouseButton, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "left":
		return input.Left, nil
	case "right":
		return input.Right, nil
	case "middle":
		return input.Middle, nil
	case "back":
		return input.Back, nil
	case "forward":
		return input.Forward, nil
	default:
		return input.None, fmt.Errorf("browser: button must be left, right, middle, back, or forward")
	}
}

func inputModifiers(raw []string) ([]input.Modifier, error) {
	out := make([]input.Modifier, 0, len(raw))
	for _, value := range raw {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "alt":
			out = append(out, input.ModifierAlt)
		case "control", "ctrl":
			out = append(out, input.ModifierCtrl)
		case "meta", "command", "cmd":
			out = append(out, input.ModifierMeta)
		case "shift":
			out = append(out, input.ModifierShift)
		case "controlormeta":
			if runtime.GOOS == "darwin" {
				out = append(out, input.ModifierMeta)
			} else {
				out = append(out, input.ModifierCtrl)
			}
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

func browserEditingCommand(command string) chromedp.KeyOption {
	return func(event *input.DispatchKeyEventParams) *input.DispatchKeyEventParams {
		if event.Type == input.KeyDown || event.Type == input.KeyRawDown {
			event.Commands = []string{command}
		}
		return event
	}
}

func browserKeyOptions(raw string, modifiers input.Modifier) []chromedp.KeyOption {
	options := []chromedp.KeyOption{chromedp.KeyModifiers(modifiers)}
	if isModifierChord(raw, "a") {
		options = append(options, browserEditingCommand("selectAll"))
	}
	return options
}

func browserKey(raw string) (string, input.Modifier) {
	parts := strings.Split(strings.TrimSpace(raw), "+")
	if len(parts) == 0 {
		return "", 0
	}
	out := ""
	var modifiers input.Modifier
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "control", "ctrl":
			modifiers |= input.ModifierCtrl
		case "shift":
			modifiers |= input.ModifierShift
		case "alt", "option":
			modifiers |= input.ModifierAlt
		case "meta", "command", "cmd":
			modifiers |= input.ModifierMeta
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

func controlOrMetaModifier() input.Modifier {
	if runtime.GOOS == "darwin" {
		return input.ModifierMeta
	}
	return input.ModifierCtrl
}
