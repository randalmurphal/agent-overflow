package main

import (
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/wake"
)

// The progress wake (K1): a gate took a `notify:`-decorated route and the run
// continued. The engine announces it and forgets it; everything below is the
// app resolving the run record into a message, exactly as it does for a resting
// run.
//
// Three properties are deliberate and load-bearing:
//
//   - It never touches the run. Every failure here is logged and dropped, and
//     the work happens on the wake queue rather than the engine's command
//     goroutine, so a progress wake cannot park, fail, or delay the run it is
//     reporting on.
//   - It is delivered to the ROOT's bound thread and names the run whose gate
//     fired, mirroring a descendant's park. A called run neither binds nor
//     notifies as itself (§5), so a campaign's wave reports at the surface the
//     supervising agent is actually watching.
//   - An UNBOUND root is inert. There is no thread to wake, and progress is not
//     an interruption: the OS notification and the overlay badge exist for runs
//     that need a human, and a run that is merely getting on with it must not
//     ring anybody's desktop. The event is still emitted on the wire, so a
//     future overlay surface has it without this path inventing one.

// afterWorkflowGateNotify is the app-side handler for engine notify events. It
// runs on the emitting goroutine, so it does nothing but hand the work over.
func (a *App) afterWorkflowGateNotify(event engine.NotifyEvent) {
	a.workflowWake.Go(func() { a.surfaceWorkflowGateNotify(event) })
}

func (a *App) surfaceWorkflowGateNotify(event engine.NotifyEvent) {
	item, err := a.store.GetWorkItem(event.ItemID)
	if err != nil {
		log.Printf("workflow progress wake %s: load run: %v", event.ItemID, err)
		return
	}
	root := item
	gate := wake.ProgressGate{
		ItemID: item.ID, WorkflowID: item.WorkflowID,
		PhaseID: event.PhaseID, Attempt: event.Attempt,
		Decision: event.Decision, Target: event.Target,
	}
	if item.ParentItemID != "" {
		chain, err := a.workflowAncestry(item)
		if err != nil {
			log.Printf("workflow progress wake %s: resolve root: %v", item.ID, err)
			return
		}
		root = chain[0]
		gate.Depth = item.CallDepth - root.CallDepth
		gate.Chain = make([]string, 0, len(chain))
		for _, ancestor := range chain {
			gate.Chain = append(gate.Chain, ancestor.ID)
		}
	}
	if root.OriginThreadID == "" {
		return
	}
	input := wake.ProgressInput{
		Run: wake.ProgressRun{
			ItemID: root.ID, Goal: root.Goal, WorkflowID: root.WorkflowID,
			WorktreePath: root.WorktreePath, Branch: root.Branch,
		},
		Gate: gate,
	}
	input.Outputs, input.OutputOverflow = a.workflowGateOutputs(item, event)
	message := wake.ComposeProgress(input)
	if len(message) > maxWakeMessageBytes {
		log.Printf("workflow progress wake %s: composed %d bytes; maximum is %d",
			root.ID, len(message), maxWakeMessageBytes)
		return
	}
	a.deliverWorkflowWake(root, composedWake{signature: wake.ProgressSignature(input), message: message})
}

// workflowGateOutputs digests the envelope the gate decided on. It is what
// makes the message worth sending — "wave 12 finished" with its verdict beats a
// heartbeat — and it reuses `run inspect`'s bounding so the same outputs read
// the same way wherever a reader meets them.
//
// An attempt whose record cannot be read still sends the message: the fact that
// the run passed this gate is true regardless, and the outputs are the
// elaboration.
func (a *App) workflowGateOutputs(item store.WorkItem, event engine.NotifyEvent) ([]wake.Output, int) {
	timeline, err := a.store.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		log.Printf("workflow progress wake %s: list phase timeline: %v", item.ID, err)
		return nil, 0
	}
	for _, phase := range timeline {
		if phase.PhaseID != event.PhaseID || phase.Attempt != event.Attempt {
			continue
		}
		outputs, err := workflowAttemptOutputs(item.ID, phase.PhaseID, phase.Attempt, phase.OutputEnvelope)
		if err != nil {
			log.Printf("workflow progress wake %s: digest gate outputs: %v", item.ID, err)
			return nil, 0
		}
		digest, overflow := workflowOutputDigest(outputs)
		projected := make([]wake.Output, 0, len(digest))
		for _, output := range digest {
			projected = append(projected, wake.Output{Name: output.Name, Value: output.Value})
		}
		return projected, overflow
	}
	return nil, 0
}
