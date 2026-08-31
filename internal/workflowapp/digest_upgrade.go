package workflowapp

import (
	"bytes"
	"context"
	"encoding/json"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

const digestUpgradeConcurrency = 2

func (s *Service) ConfigureDigestGeneratorForTesting(
	generate func(context.Context, store.WorkItem, Digest) (Digest, error),
) {
	s.deps.GenerateDigest = generate
}

func (s *Service) queueDigestUpgrade(item store.WorkItem, template Digest, expected []byte) {
	s.digestMu.Lock()
	if s.digestSlots == nil {
		s.digestSlots = make(chan struct{}, digestUpgradeConcurrency)
	}
	slots := s.digestSlots
	select {
	case slots <- struct{}{}:
	default:
		s.digestMu.Unlock()
		s.deps.Logf("workflow digest %s: async upgrade skipped because both generator slots are busy", item.ID)
		return
	}
	s.digestMu.Unlock()
	go func() {
		defer func() { <-slots }()
		if s.deps.Context().Err() != nil {
			return
		}
		s.upgradeDigest(item, template, expected)
	}()
}

func (s *Service) upgradeDigest(item store.WorkItem, template Digest, expected []byte) {
	if s.deps.GenerateDigest == nil {
		return
	}
	generated, err := s.deps.GenerateDigest(s.deps.Context(), item, template)
	if err != nil {
		s.deps.Logf("workflow digest %s: async upgrade: %v", item.ID, err)
		return
	}
	current, err := s.deps.Store.GetWorkItem(item.ID)
	if err != nil {
		s.deps.Logf("workflow digest %s: reload before upgrade: %v", item.ID, err)
		return
	}
	if current.State != item.State || current.Reason != item.Reason || !bytes.Equal(current.Digest, expected) {
		return
	}
	encoded, err := json.Marshal(generated)
	if err != nil {
		s.deps.Logf("workflow digest %s: encode upgrade: %v", item.ID, err)
		return
	}
	if err := s.deps.Store.UpdateWorkItemDigest(item.ID, encoded); err != nil {
		s.deps.Logf("workflow digest %s: persist upgrade: %v", item.ID, err)
		return
	}
	if s.deps.EmitState != nil {
		s.deps.EmitState(engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID,
			From: engine.State(item.State), To: engine.State(item.State), Reason: engine.Reason(item.Reason),
		})
	}
}

func (s *Service) DigestUpgradesIdle() bool {
	s.digestMu.Lock()
	defer s.digestMu.Unlock()
	return len(s.digestSlots) == 0
}
