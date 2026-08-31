package highlightapp

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
)

const (
	seedMaxScanBytes           = 1 << 20
	seedMaxSourceBytes         = 128 << 10
	seedMaxStates              = 64
	seedMaxEphemeralWorkers    = 8
	persistedCodeSpansMaxBytes = 256 << 10
)

type SeedEvent struct {
	ThreadID   string
	ItemID     string
	Lang       string
	ContentKey string
	LineHashes []uint32
	Lines      []highlight.EncodedLine
	Final      bool
}

type PersistedCodeSpan struct {
	Lang       string                  `json:"lang"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
}

type PersistedCodeSpans struct {
	Version string              `json:"hv"`
	Blocks  []PersistedCodeSpan `json:"blocks"`
}

type seeder struct {
	mu               sync.Mutex
	states           map[string]*seedState
	ephemeralWorkers atomic.Int32
}
type seedState struct {
	fencesDone int
	busy       bool
	pending    seedTick
	hasPending bool
}
type seedTick struct {
	threadID, itemID, text string
	final                  bool
}

func (s *Service) ObserveAssistantText(threadID, itemID, text string, final bool) {
	hasRemote := s.config.HasRemoteClient()
	seed := &s.seeder
	key := threadID + "|" + itemID
	seed.mu.Lock()
	defer seed.mu.Unlock()
	state := seed.states[key]
	if !hasRemote {
		if state != nil && final {
			delete(seed.states, key)
		}
		return
	}
	ephemeral := false
	if state == nil {
		state = &seedState{}
		switch {
		case final:
			if seed.ephemeralWorkers.Add(1) > seedMaxEphemeralWorkers {
				seed.ephemeralWorkers.Add(-1)
				return
			}
			ephemeral = true
		case len(seed.states) >= seedMaxStates:
			return
		default:
			if seed.states == nil {
				seed.states = make(map[string]*seedState)
			}
			seed.states[key] = state
		}
	}
	state.pending = seedTick{threadID: threadID, itemID: itemID, text: text, final: final}
	state.hasPending = true
	if state.busy {
		return
	}
	state.busy = true
	if ephemeral {
		go func() { defer seed.ephemeralWorkers.Add(-1); s.runSeedWorker(key, state) }()
		return
	}
	go s.runSeedWorker(key, state)
}

func (s *Service) runSeedWorker(key string, state *seedState) {
	seed := &s.seeder
	for {
		seed.mu.Lock()
		if !state.hasPending {
			state.busy = false
			seed.mu.Unlock()
			return
		}
		tick := state.pending
		state.hasPending = false
		if tick.final {
			if seed.states[key] == state {
				delete(seed.states, key)
			}
			state.busy = false
		}
		seed.mu.Unlock()
		s.processSeedTick(state, tick)
		if tick.final {
			return
		}
	}
}

func (s *Service) processSeedTick(state *seedState, tick seedTick) {
	if len(tick.text) > seedMaxScanBytes || !s.config.HasRemoteClient() {
		return
	}
	fences := highlight.ScanFences(tick.text)
	if len(fences) < state.fencesDone {
		state.fencesDone = 0
	}
	for i := state.fencesDone; i < len(fences); i++ {
		fence := fences[i]
		finalFence := fence.Closed || tick.final
		if finalFence {
			state.fencesDone = i + 1
		}
		if fence.Lang == "" || len(fence.Source) > seedMaxSourceBytes || !utf8.ValidString(fence.Source) {
			continue
		}
		lang := highlight.LangFromName(fence.Lang)
		var res highlight.Result
		if finalFence {
			res = s.cache.Code(lang, fence.Source)
		} else {
			res = s.cache.CodeTransient(lang, fence.Source)
		}
		if res.Incomplete {
			continue
		}
		event := SeedEvent{ThreadID: tick.threadID, ItemID: tick.itemID, Lang: fence.Lang,
			LineHashes: highlight.FrontendLineHashes(fence.Source), Lines: res.Lines, Final: finalFence}
		if finalFence {
			event.ContentKey = highlight.FrontendContentKey(fence.Source)
		}
		s.config.EmitSeed(event)
	}
}

func (s *Service) PurgeThread(threadID string) {
	prefix := threadID + "|"
	s.seeder.mu.Lock()
	defer s.seeder.mu.Unlock()
	for key := range s.seeder.states {
		if strings.HasPrefix(key, prefix) {
			delete(s.seeder.states, key)
		}
	}
}

func (s *Service) BuildPersistedCodeSpans(text string) json.RawMessage {
	if text == "" || len(text) > seedMaxScanBytes {
		return nil
	}
	var blocks []PersistedCodeSpan
	budget := persistedCodeSpansMaxBytes
	for _, fence := range highlight.ScanFences(text) {
		if fence.Lang == "" || len(fence.Source) > seedMaxSourceBytes || !utf8.ValidString(fence.Source) {
			continue
		}
		res := s.cache.Code(highlight.LangFromName(fence.Lang), fence.Source)
		if res.Incomplete {
			continue
		}
		cost := encodedLinesBytes(res.Lines)
		if cost > budget {
			continue
		}
		budget -= cost
		blocks = append(blocks, PersistedCodeSpan{Lang: fence.Lang, ContentKey: highlight.FrontendContentKey(fence.Source), Lines: res.Lines})
	}
	if len(blocks) == 0 {
		return nil
	}
	blob, err := json.Marshal(PersistedCodeSpans{Version: highlight.SchemaVersion(), Blocks: blocks})
	if err != nil {
		log.Printf("highlightapp: marshal persisted code spans: %v", err)
		return nil
	}
	return blob
}

func encodedLinesBytes(lines []highlight.EncodedLine) int {
	bytes := 0
	for _, line := range lines {
		bytes += 8 + len(line.Runs)*4
	}
	return bytes
}
