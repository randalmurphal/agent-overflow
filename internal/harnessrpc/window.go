package harnessrpc

import "errors"

// WindowRect is one device-independent-pixel rectangle in desktop screen
// coordinates. It deliberately mirrors neither Wails nor windowgeom so the
// harness wire stays owned by this package.
type WindowRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// WindowScreen is the display geometry associated with a window snapshot.
// Bounds and WorkArea are DIPs; PhysicalWorkArea is device pixels.
type WindowScreen struct {
	ID               string     `json:"id"`
	Bounds           WindowRect `json:"bounds"`
	WorkArea         WindowRect `json:"workArea"`
	PhysicalWorkArea WindowRect `json:"physicalWorkArea"`
	ScaleFactor      float32    `json:"scaleFactor"`
}

// WindowState is a point-in-time read of the isolated native window. A caller
// can compare it with settings.json after a restart without relying on OS
// accessibility privileges.
type WindowState struct {
	Bounds     WindowRect   `json:"bounds"`
	Maximized  bool         `json:"maximized"`
	Fullscreen bool         `json:"fullscreen"`
	Minimized  bool         `json:"minimized"`
	Screen     WindowScreen `json:"screen"`
}

// WindowCommand drives one native state transition or sets the outer bounds.
// Exactly one of Action and Bounds must be supplied.
type WindowCommand struct {
	Action string      `json:"action,omitempty"`
	Bounds *WindowRect `json:"bounds,omitempty"`
}

func (c WindowCommand) validate() error {
	if (c.Action == "") == (c.Bounds == nil) {
		return errors.New("window command requires exactly one of action or bounds")
	}
	if c.Bounds != nil {
		if c.Bounds.Width <= 0 || c.Bounds.Height <= 0 {
			return errors.New("window bounds require positive width and height")
		}
		return nil
	}
	switch c.Action {
	case "maximize", "unmaximize", "fullscreen", "unfullscreen", "minimize", "unminimize":
		return nil
	default:
		return errors.New("unknown window action: use maximize, unmaximize, fullscreen, unfullscreen, minimize, or unminimize")
	}
}

// WindowController is implemented by the executable shell, which alone owns
// the Wails window. The receiver is constructed before that window exists, so
// implementations must resolve it lazily and return an error while headless or
// before creation.
type WindowController interface {
	State() (WindowState, error)
	Command(WindowCommand) error
}

// HarnessWindowState reports the real native window's outer bounds, state, and
// display geometry. It is available only on a windowed isolated boot.
func (h *Harness) HarnessWindowState() (WindowState, error) {
	if h == nil || h.config.Window == nil {
		return WindowState{}, errors.New("native window unavailable: start the harness with --window")
	}
	return h.config.Window.State()
}

// HarnessWindowCommand drives the real native window. State transitions are
// asynchronous on platforms that animate them; callers should poll
// HarnessWindowState for the settled result.
func (h *Harness) HarnessWindowCommand(command WindowCommand) error {
	if h == nil || h.config.Window == nil {
		return errors.New("native window unavailable: start the harness with --window")
	}
	if err := command.validate(); err != nil {
		return err
	}
	return h.config.Window.Command(command)
}
