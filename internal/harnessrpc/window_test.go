package harnessrpc

import (
	"errors"
	"strings"
	"testing"
)

type fakeWindowController struct {
	state    WindowState
	stateErr error
	commands []WindowCommand
}

func (f *fakeWindowController) State() (WindowState, error) { return f.state, f.stateErr }

func (f *fakeWindowController) Command(command WindowCommand) error {
	f.commands = append(f.commands, command)
	return nil
}

func TestHarnessWindowStateRequiresWindowedBoot(t *testing.T) {
	h := New(Config{})
	if _, err := h.HarnessWindowState(); err == nil || !strings.Contains(err.Error(), "--window") {
		t.Fatalf("HarnessWindowState error = %v, want --window refusal", err)
	}
	if err := h.HarnessWindowCommand(WindowCommand{Action: "maximize"}); err == nil || !strings.Contains(err.Error(), "--window") {
		t.Fatalf("HarnessWindowCommand error = %v, want --window refusal", err)
	}
}

func TestHarnessWindowStateReturnsNativeSnapshot(t *testing.T) {
	want := WindowState{
		Bounds:    WindowRect{X: 100, Y: 80, Width: 1100, Height: 720},
		Maximized: true,
		Screen: WindowScreen{
			ID:               "display-1",
			WorkArea:         WindowRect{X: 0, Y: 33, Width: 1470, Height: 923},
			PhysicalWorkArea: WindowRect{X: 0, Y: 66, Width: 2940, Height: 1846},
			ScaleFactor:      2,
		},
	}
	controller := &fakeWindowController{state: want}
	h := New(Config{Window: controller})

	got, err := h.HarnessWindowState()
	if err != nil {
		t.Fatalf("HarnessWindowState: %v", err)
	}
	if got != want {
		t.Fatalf("HarnessWindowState = %+v, want %+v", got, want)
	}

	controller.stateErr = errors.New("snapshot failed")
	if _, err := h.HarnessWindowState(); err == nil || err.Error() != "snapshot failed" {
		t.Fatalf("HarnessWindowState propagated error = %v", err)
	}
}

func TestHarnessWindowCommandValidatesBeforeDriving(t *testing.T) {
	controller := &fakeWindowController{}
	h := New(Config{Window: controller})

	invalid := []WindowCommand{
		{},
		{Action: "maximize", Bounds: &WindowRect{Width: 100, Height: 100}},
		{Action: "zoom"},
		{Bounds: &WindowRect{Width: 0, Height: 100}},
	}
	for _, command := range invalid {
		if err := h.HarnessWindowCommand(command); err == nil {
			t.Errorf("HarnessWindowCommand(%+v) unexpectedly succeeded", command)
		}
	}
	if len(controller.commands) != 0 {
		t.Fatalf("invalid commands reached controller: %+v", controller.commands)
	}

	bounds := WindowRect{X: 8, Y: 41, Width: 1454, Height: 907}
	valid := []WindowCommand{{Action: "maximize"}, {Bounds: &bounds}}
	for _, command := range valid {
		if err := h.HarnessWindowCommand(command); err != nil {
			t.Fatalf("HarnessWindowCommand(%+v): %v", command, err)
		}
	}
	if len(controller.commands) != len(valid) {
		t.Fatalf("controller received %d commands, want %d", len(controller.commands), len(valid))
	}
}
