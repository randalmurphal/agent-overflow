package closer

import (
	"errors"
	"strings"
	"testing"
)

func TestStackAddIgnoresNil(t *testing.T) {
	var s Stack
	s.Add(nil)
	if len(s) != 0 {
		t.Fatalf("len(s) = %d, want 0 — nil cleanup must be dropped", len(s))
	}
}

func TestStackRunReverseOrder(t *testing.T) {
	order := []string{}
	var s Stack
	s.Add(func() error { order = append(order, "first"); return nil })
	s.Add(func() error { order = append(order, "second"); return nil })
	s.Add(func() error { order = append(order, "third"); return nil })

	if err := s.Run(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	want := []string{"third", "second", "first"}
	for i, got := range order {
		if got != want[i] {
			t.Fatalf("order[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestStackRunJoinsErrors(t *testing.T) {
	first := errors.New("first cleanup err")
	third := errors.New("third cleanup err")
	var s Stack
	s.Add(func() error { return first })
	s.Add(func() error { return nil })
	s.Add(func() error { return third })

	err := s.Run()
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	if !errors.Is(err, first) || !errors.Is(err, third) {
		t.Fatalf("errors.Is missed a cause: %v", err)
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "third") {
		t.Fatalf("joined message = %q, want both causes", err.Error())
	}
}

func TestStackRunEmpty(t *testing.T) {
	var s Stack
	if err := s.Run(); err != nil {
		t.Fatalf("empty Stack.Run() = %v, want nil", err)
	}
}
