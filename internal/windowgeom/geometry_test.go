package windowgeom

import "testing"

var (
	primary   = Rect{X: 0, Y: 0, Width: 1920, Height: 1080}
	secondary = Rect{X: 1920, Y: 0, Width: 1920, Height: 1080}
)

func TestClampRejectsUnrestorable(t *testing.T) {
	cases := map[string]struct {
		g       Geometry
		screens []Rect
	}{
		"never saved":   {Geometry{X: 10, Y: 10, Width: 800, Height: 600, Valid: false}, []Rect{primary}},
		"zero width":    {Geometry{X: 10, Y: 10, Width: 0, Height: 600, Valid: true}, []Rect{primary}},
		"zero height":   {Geometry{X: 10, Y: 10, Width: 800, Height: 0, Valid: true}, []Rect{primary}},
		"no screens":    {Geometry{X: 10, Y: 10, Width: 800, Height: 600, Valid: true}, nil},
		"off all":       {Geometry{X: 5000, Y: 5000, Width: 800, Height: 600, Valid: true}, []Rect{primary}},
		"unplugged 2nd": {Geometry{X: 2000, Y: 100, Width: 800, Height: 600, Display: secondary, Valid: true}, []Rect{primary}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := tc.g.Clamp(tc.screens); ok {
				t.Fatalf("Clamp() ok = true (got %+v), want false", got)
			}
		})
	}
}

func TestClampKeepsFullyVisibleWindow(t *testing.T) {
	g := Geometry{X: 100, Y: 100, Width: 800, Height: 600, Valid: true}
	got, ok := g.Clamp([]Rect{primary})
	if !ok {
		t.Fatal("Clamp() ok = false, want true")
	}
	if got.X != 100 || got.Y != 100 || got.Width != 800 || got.Height != 600 {
		t.Fatalf("Clamp() = %+v, want unchanged 100,100,800,600", got)
	}
	if got.Display != primary {
		t.Fatalf("Clamp() Display = %+v, want %+v", got.Display, primary)
	}
}

func TestClampAnchorsToSavedDisplayWhenStillPresent(t *testing.T) {
	g := Geometry{X: 2000, Y: 100, Width: 800, Height: 600, Display: secondary, Valid: true}
	got, ok := g.Clamp([]Rect{primary, secondary})
	if !ok {
		t.Fatal("Clamp() ok = false, want true")
	}
	if got.X != 2000 || got.Y != 100 {
		t.Fatalf("Clamp() = %+v, want kept on secondary at 2000,100", got)
	}
}

func TestClampPullsStaleRectOntoSavedDisplay(t *testing.T) {
	// The Tracker can persist a normal rect from screen A with Display pointing
	// at screen B: the window changed screens while maximized/fullscreen, so
	// only the flags and Display moved. Restore must land on B (where the
	// window actually was), pulling the stale rect inside B's work area.
	g := Geometry{X: 100, Y: 120, Width: 800, Height: 600, Display: secondary, Valid: true}
	got, ok := g.Clamp([]Rect{primary, secondary})
	if !ok {
		t.Fatal("Clamp() ok = false, want true")
	}
	if got.Display != secondary {
		t.Fatalf("Clamp() Display = %+v, want %+v", got.Display, secondary)
	}
	if got.X < secondary.X || got.X+got.Width > secondary.X+secondary.Width ||
		got.Y < secondary.Y || got.Y+got.Height > secondary.Y+secondary.Height {
		t.Fatalf("Clamp() = %+v, want fully inside secondary %+v", got, secondary)
	}
}

func TestClampCapsOversizeToScreen(t *testing.T) {
	g := Geometry{X: 0, Y: 0, Width: 5000, Height: 5000, Valid: true}
	got, ok := g.Clamp([]Rect{primary})
	if !ok {
		t.Fatal("Clamp() ok = false, want true")
	}
	if got.Width != primary.Width || got.Height != primary.Height {
		t.Fatalf("Clamp() size = %dx%d, want %dx%d", got.Width, got.Height, primary.Width, primary.Height)
	}
	if got.X != 0 || got.Y != 0 {
		t.Fatalf("Clamp() origin = %d,%d, want 0,0", got.X, got.Y)
	}
}

func TestClampShiftsPartlyOffscreenWindowFullyInside(t *testing.T) {
	// Window hangs off the right edge of the primary screen.
	g := Geometry{X: 1900, Y: 100, Width: 800, Height: 600, Valid: true}
	got, ok := g.Clamp([]Rect{primary})
	if !ok {
		t.Fatal("Clamp() ok = false, want true")
	}
	if got.X+got.Width > primary.Width {
		t.Fatalf("Clamp() X+W = %d, want <= %d (fully on-screen)", got.X+got.Width, primary.Width)
	}
	if got.X != primary.Width-got.Width {
		t.Fatalf("Clamp() X = %d, want %d (flush to right edge)", got.X, primary.Width-got.Width)
	}
}
