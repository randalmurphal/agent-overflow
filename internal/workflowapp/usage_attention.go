package workflowapp

import (
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"

	"github.com/google/uuid"
)

// UsageAttentionSurface identifies the outage and watcher a successful
// or suppressed attention decision covered.
type UsageAttentionSurface struct {
	ScopeID  store.WorkflowProviderUsageScopeID
	ThreadID string
}

type UsageAttentionDecision struct {
	Surface  UsageAttentionSurface
	Claim    *store.WorkflowProviderUsageAttentionClaim
	Suppress bool
}

// UsageAttentionForRest claims the one notification generation shared
// by usage-limited runs under the same provider-account scope and watching
// thread. The run remains fully visible in the workflow overlay either way;
// Suppress only suppresses another injected message and OS notification for an
// outage the watcher has already been told about.
func (s *Service) UsageAttentionForRest(root, parked store.WorkItem) UsageAttentionDecision {
	if root.OriginThreadID == "" {
		return UsageAttentionDecision{} // unbound runs keep ordinary per-run OS attention
	}
	scopeID, err := s.workflowRestingUsageScope(parked)
	if err != nil {
		s.deps.Logf("workflow usage attention %s: resolve failure scope: %v; delivering normally", parked.ID, err)
		return UsageAttentionDecision{}
	}
	if scopeID == 0 {
		return UsageAttentionDecision{}
	}
	decision := UsageAttentionDecision{
		Surface: UsageAttentionSurface{ScopeID: scopeID, ThreadID: root.OriginThreadID},
	}
	claim, claimed, err := s.deps.Store.ClaimWorkflowProviderUsageAttention(
		scopeID, root.OriginThreadID, parked.ID, uuid.NewString(), s.deps.Now().UnixMilli(),
	)
	if err != nil {
		s.deps.Logf("workflow usage attention %s: claim scope %d for thread %s: %v; delivering normally",
			parked.ID, scopeID, root.OriginThreadID, err)
		return UsageAttentionDecision{}
	}
	if !claimed {
		s.deps.Logf("workflow usage attention %s: suppressed another notification for provider usage scope %d watched by thread %s",
			parked.ID, scopeID, root.OriginThreadID)
		decision.Suppress = true
		return decision
	}
	decision.Claim = &claim
	return decision
}

// workflowRestingUsageScope returns a scope only when usage limits account for
// the whole actionable park. A mixed fan-out failure is intentionally not
// coalesced: hiding a genuine non-usage failure behind a familiar provider
// outage would make the run's only notification misleading.
func (s *Service) workflowRestingUsageScope(item store.WorkItem) (store.WorkflowProviderUsageScopeID, error) {
	switch engine.Reason(item.Reason) {
	case engine.ReasonProviderUsageLimited:
		phase, _, err := s.workflowRestingPhase(item.ID)
		if err != nil {
			return 0, err
		}
		return phase.ProviderUsageScopeID, nil
	case engine.ReasonUnitFailed:
		failed, err := s.FailedUnits(item.ID)
		if err != nil {
			return 0, err
		}
		var scopeID store.WorkflowProviderUsageScopeID
		for _, unit := range failed {
			if unit.ProviderUsageScopeID == 0 {
				return 0, nil
			}
			if scopeID == 0 {
				scopeID = unit.ProviderUsageScopeID
				continue
			}
			if scopeID != unit.ProviderUsageScopeID {
				return 0, nil
			}
		}
		return scopeID, nil
	default:
		return 0, nil
	}
}

func (s *Service) promoteWorkflowUsageAttention(claim store.WorkflowProviderUsageAttentionClaim) {
	promoted, err := s.deps.Store.PromoteWorkflowProviderUsageAttention(claim, s.deps.Now().UnixMilli())
	if err != nil {
		s.deps.Logf("workflow usage attention: promote scope %d generation %d for thread %s: %v",
			claim.ScopeID, claim.Generation, claim.ThreadID, err)
		// A failed settlement must not strand a queued marker that suppresses
		// later parks. Release is another best-effort write; if storage recovered
		// between the two operations, the fallback is a duplicate rather than
		// silence.
		s.releaseWorkflowUsageAttention(&claim)
		return
	}
	if !promoted {
		s.deps.Logf("workflow usage attention: queued scope %d generation %d for thread %s was re-armed before delivery; not marking the current generation delivered",
			claim.ScopeID, claim.Generation, claim.ThreadID)
	}
}

func (s *Service) releaseWorkflowUsageAttention(claim *store.WorkflowProviderUsageAttentionClaim) {
	if claim == nil {
		return
	}
	released, err := s.deps.Store.ReleaseWorkflowProviderUsageAttention(*claim, s.deps.Now().UnixMilli())
	if err != nil {
		s.deps.Logf("workflow usage attention: release scope %d generation %d for thread %s: %v",
			claim.ScopeID, claim.Generation, claim.ThreadID, err)
		return
	}
	if !released {
		s.deps.Logf("workflow usage attention: scope %d generation %d for thread %s was already re-armed while its failed delivery settled",
			claim.ScopeID, claim.Generation, claim.ThreadID)
	}
}

// reclaimWorkflowUsageAttention transfers prior-process claims before the
// engine starts emitting this process's recovery transitions. That ordering is
// load-bearing: reclaiming after Start could steal a claim whose in-memory
// delivery callback belongs to the new process.
func (s *Service) ReclaimUsageAttention() []store.WorkflowProviderUsageAttentionRecovery {
	recoveries, err := s.deps.Store.ReclaimQueuedWorkflowProviderUsageAttention(uuid.NewString(), s.deps.Now().UnixMilli())
	if err != nil {
		s.deps.Logf("workflow usage attention boot sweep: %v", err)
		return nil
	}
	return recoveries
}

// surfaceReclaimedWorkflowUsageAttention runs after engine recovery rebuilt the
// item rows. Recovery is by durable scope and watcher, not only the original
// source, which may have resolved while a suppressed sibling remains parked.
func (s *Service) SurfaceReclaimedUsageAttention(recoveries []store.WorkflowProviderUsageAttentionRecovery) {
	for _, recovery := range recoveries {
		s.wake.Go(func() { s.recoverWorkflowUsageAttention(recovery) })
	}
}

func (s *Service) recoverWorkflowUsageAttention(recovery store.WorkflowProviderUsageAttentionRecovery) {
	claim := recovery.Claim
	itemIDs, err := s.deps.Store.ListWorkflowProviderUsageAffectedItemIDs(claim.ScopeID)
	if err != nil {
		s.deps.Logf("workflow usage attention boot recovery: scope %d watched by thread %s: %v",
			claim.ScopeID, claim.ThreadID, err)
		return
	}
	// Prefer the original source while it is still affected. This preserves the
	// message a no-race restart would have delivered without making that source
	// the correctness boundary.
	for index, itemID := range itemIDs {
		if itemID == recovery.SourceItemID {
			itemIDs[0], itemIDs[index] = itemIDs[index], itemIDs[0]
			break
		}
	}
	for _, itemID := range itemIDs {
		item, err := s.deps.Store.GetWorkItem(itemID)
		if err != nil {
			s.deps.Logf("workflow usage attention boot recovery: load candidate %s: %v", itemID, err)
			continue
		}
		scopeID, err := s.workflowRestingUsageScope(item)
		if err != nil {
			s.deps.Logf("workflow usage attention boot recovery: resolve candidate %s scope: %v", itemID, err)
			continue
		}
		if scopeID != claim.ScopeID {
			continue
		}
		chain, err := s.Ancestry(item)
		if err != nil {
			s.deps.Logf("workflow usage attention boot recovery: resolve candidate %s watcher: %v", itemID, err)
			continue
		}
		root := chain[0]
		if root.OriginThreadID != claim.ThreadID || root.ID != item.ID && root.State != string(engine.StateRunning) {
			continue
		}
		reassigned, err := s.deps.Store.ReassignWorkflowProviderUsageAttentionSource(claim, itemID, s.deps.Now().UnixMilli())
		if err != nil {
			s.deps.Logf("workflow usage attention boot recovery: reassign scope %d watched by thread %s to %s: %v",
				claim.ScopeID, claim.ThreadID, itemID, err)
			return
		}
		if !reassigned {
			// A concurrent action or recovery replaced this claim. It owns the
			// current generation now; this stale recovery must not release it.
			return
		}
		// The source reassignment is durable before delivery. A second crash now
		// recovers this candidate, while a concurrent transition is caught by the
		// reload and validation inside the surface helper below.
		if surfaced := s.surfaceRecoveredWorkflowUsageAttention(itemID, claim); surfaced.ScopeID == claim.ScopeID && surfaced.ThreadID == claim.ThreadID {
			return
		}
	}
	// No currently affected run remains under this scope/watcher. Clear the
	// transferred claim so a genuinely new refusal can announce itself.
	s.releaseWorkflowUsageAttention(&claim)
}

func (s *Service) surfaceRecoveredWorkflowUsageAttention(itemID string, claim store.WorkflowProviderUsageAttentionClaim) UsageAttentionSurface {
	item, err := s.deps.Store.GetWorkItem(itemID)
	if err != nil {
		s.deps.Logf("workflow usage attention boot recovery: reload candidate %s: %v", itemID, err)
		return UsageAttentionSurface{}
	}
	scopeID, err := s.workflowRestingUsageScope(item)
	if err != nil {
		s.deps.Logf("workflow usage attention boot recovery: revalidate candidate %s scope: %v", itemID, err)
		return UsageAttentionSurface{}
	}
	chain, err := s.Ancestry(item)
	if err != nil {
		s.deps.Logf("workflow usage attention boot recovery: revalidate candidate %s watcher: %v", itemID, err)
		return UsageAttentionSurface{}
	}
	root := chain[0]
	if scopeID != claim.ScopeID || root.OriginThreadID != claim.ThreadID ||
		root.ID != item.ID && root.State != string(engine.StateRunning) {
		return UsageAttentionSurface{}
	}
	usage := UsageAttentionDecision{
		Surface: UsageAttentionSurface{ScopeID: claim.ScopeID, ThreadID: claim.ThreadID},
		Claim:   &claim,
	}
	if item.ParentItemID != "" {
		return s.surfaceResolvedDescendantPark(item, chain, usage)
	}
	return s.surfaceRootRestingWorkflowItem(item, usage)
}

func (s *Service) ReleaseUsageAttentionForThread(threadID string) {
	if err := s.deps.Store.ReleaseQueuedWorkflowProviderUsageAttentionForThread(threadID, s.deps.Now().UnixMilli()); err != nil {
		s.deps.Logf("workflow usage attention: release queued claims for closing thread %s: %v", threadID, err)
	}
}
