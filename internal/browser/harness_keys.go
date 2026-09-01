package browser

import (
	"context"
	"fmt"

	"agent-overflow/internal/keybindings"
)

// engineKeyPress is the harness-only injection seam: deliver one chord to a
// page's native view through the window's own event path. Only an engine
// with a real in-process view implements it; the fake engine and the
// launcher-hosted engine refuse by name.
type engineKeyPress interface {
	PressKey(handle string, chord keybindings.Accelerator) error
}

// HarnessPressKey is the Harness receiver's door to engineKeyPress. It is
// what makes the keyboard path (browser-tools.md § Keyboard) testable on a
// machine where the driver holds no Accessibility trust: the app posts the
// key event into its own window, and the chord gate sees a real NSEvent.
func (m *Manager) HarnessPressKey(ctx context.Context, access Access, pageID string, chord keybindings.Accelerator) error {
	engine, ok := m.engine.(engineKeyPress)
	if !ok {
		return fmt.Errorf("browser: key injection is not available on this browser engine")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return err
	}
	return engine.PressKey(p.driver.Handle(), chord)
}
