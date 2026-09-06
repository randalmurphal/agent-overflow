package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/workflow/def"
)

func TestWorkAdmissionProtectsNewAndResumedRuns(t *testing.T) {
	var allowed atomic.Bool
	refused := errors.New("host maintenance")
	h := newHarness(t, Config{BeginWork: func(context.Context) (func(), error) {
		if !allowed.Load() {
			return nil, refused
		}
		return func() {}, nil
	}}, map[string]def.Workflow{"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}})}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); !errors.Is(err, refused) {
		t.Fatalf("new run: %v", err)
	}
	if _, err := h.store.GetWorkItem(item.ID); err == nil {
		t.Fatal("refused admission persisted a run")
	}
	allowed.Store(true)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(item.ID); err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	if err := h.engine.ResumeItem(item.ID); !errors.Is(err, refused) {
		t.Fatalf("resume: %v", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonPaused)
	allowed.Store(true)
	if err := h.engine.ResumeItem(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
}
