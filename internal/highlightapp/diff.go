package highlightapp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/workspacepath"
)

const (
	diffSeedMaxScanBytes  = 4 << 20
	diffSeedMaxFileBytes  = 256 << 10
	diffSeedMaxTotalBytes = 1 << 20
	diffSeedMaxFiles      = 32
	diffSeedMaxWorkers    = 4
)

type PatchSpanSeed struct {
	Path       string                  `json:"path"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
	Primed     bool                    `json:"primed,omitempty"`
}
type PersistedPatchSpans struct {
	Version string          `json:"hv"`
	Files   []PatchSpanSeed `json:"files"`
}
type DiffSeedEvent struct {
	ThreadID string
	Files    []PatchSpanSeed
}

func (s *Service) computePatchSpanSeeds(patch string, prime func(string) string) []PatchSpanSeed {
	if patch == "" || len(patch) > diffSeedMaxScanBytes {
		return nil
	}
	var seeds []PatchSpanSeed
	total := 0
	for _, segment := range highlight.SplitPatchFiles(patch) {
		if len(seeds) >= diffSeedMaxFiles {
			break
		}
		if len(segment.Patch) > diffSeedMaxFileBytes || !utf8.ValidString(segment.Patch) {
			continue
		}
		if total+len(segment.Patch) > diffSeedMaxTotalBytes {
			break
		}
		total += len(segment.Patch)
		lang := highlight.LangFromPath(segment.Path)
		content := ""
		if prime != nil {
			content = prime(segment.Path)
		}
		var result highlight.Result
		primed := content != "" && highlight.PatchMatchesContent(segment.Patch, content)
		if primed {
			result = s.cache.PatchWithContext(lang, segment.Patch, content)
		} else {
			result = s.cache.Patch(lang, segment.Patch)
		}
		if result.Incomplete {
			continue
		}
		seeds = append(seeds, PatchSpanSeed{Path: segment.Path, ContentKey: highlight.FrontendContentKey(segment.Patch), Lines: result.Lines, Primed: primed})
	}
	return seeds
}

func (s *Service) workspacePrimer(threadID string) func(string) string {
	if s.config.WorkspaceForThread == nil || s.config.ReadWorkspaceFile == nil {
		return nil
	}
	workspace, err := s.config.WorkspaceForThread(threadID)
	if err != nil || workspace == "" {
		return nil
	}
	contents := map[string]string{}
	return func(path string) string {
		if content, ok := contents[path]; ok {
			return content
		}
		content := ""
		if rel, err := workspacepath.NormalizeRelative(path); err == nil {
			if read, err := s.config.ReadWorkspaceFile(filepath.Join(workspace, rel), highlight.MaxPrimeBytes); err == nil {
				content = read
			}
		}
		contents[path] = content
		return content
	}
}

func (s *Service) ObserveDiffPayload(threadID, payloadID string, previews []string, patch string) {
	if s.diffWorkers.Add(1) > diffSeedMaxWorkers {
		s.diffWorkers.Add(-1)
		return
	}
	go func() {
		defer s.diffWorkers.Add(-1)
		prime := s.workspacePrimer(threadID)
		s.persistEditSnapshots(threadID, payloadID, patch, prime)
		var previewSeeds []PatchSpanSeed
		total := 0
		for _, preview := range previews {
			if len(previewSeeds) >= diffSeedMaxFiles {
				break
			}
			if len(preview) > diffSeedMaxFileBytes {
				continue
			}
			if total+len(preview) > diffSeedMaxTotalBytes {
				break
			}
			total += len(preview)
			previewSeeds = append(previewSeeds, s.computePatchSpanSeeds(preview, prime)...)
		}
		previewSeeds = capPatchSpanSeedBytes(previewSeeds, persistedCodeSpansMaxBytes)
		persisted := s.persistPayloadSpans(threadID, payloadID, previewSeeds, s.computePatchSpanSeeds(patch, prime))
		if persisted && len(previewSeeds) > 0 {
			s.config.EmitDiffSeed(DiffSeedEvent{ThreadID: threadID, Files: previewSeeds})
		}
	}()
}

func (s *Service) persistEditSnapshots(threadID, payloadID, patch string, prime func(string) string) {
	if s.config.Store == nil || prime == nil || patch == "" || len(patch) > diffSeedMaxScanBytes {
		return
	}
	captured := 0
	for _, segment := range highlight.SplitPatchFiles(patch) {
		if captured >= diffSeedMaxFiles {
			break
		}
		content := prime(segment.Path)
		if content == "" || !highlight.PatchMatchesContent(segment.Patch, content) {
			continue
		}
		captured++
		if err := s.config.Store.PutEditFileSnapshot(threadID, payloadID, segment.Path, content, s.config.Now().UnixMilli()); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return
			}
			log.Printf("highlightapp: persist edit snapshot %s %s: %v", payloadID, segment.Path, err)
		}
	}
}

func (s *Service) persistPayloadSpans(threadID, payloadID string, preview, full []PatchSpanSeed) bool {
	if s.config.Store == nil {
		return false
	}
	previewBlob, fullBlob := marshalPersistedPatchSpans(preview), marshalPersistedPatchSpans(full)
	if previewBlob == "" && fullBlob == "" {
		return true
	}
	if err := s.config.Store.UpdatePayloadSpans(threadID, payloadID, previewBlob, fullBlob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		log.Printf("highlightapp: persist payload spans %s: %v", payloadID, err)
	}
	return true
}

func marshalPersistedPatchSpans(seeds []PatchSpanSeed) string {
	if len(seeds) == 0 {
		return ""
	}
	blob, err := json.Marshal(PersistedPatchSpans{Version: highlight.SchemaVersion(), Files: seeds})
	if err != nil {
		log.Printf("highlightapp: marshal persisted patch spans: %v", err)
		return ""
	}
	return string(blob)
}

func capPatchSpanSeedBytes(seeds []PatchSpanSeed, budget int) []PatchSpanSeed {
	kept := seeds[:0]
	for _, seed := range seeds {
		cost := encodedLinesBytes(seed.Lines)
		if cost > budget {
			continue
		}
		budget -= cost
		kept = append(kept, seed)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func (s *Service) LoadPatchSpans(threadID, payloadID string) []PatchSpanSeed {
	if s.config.Store == nil {
		return nil
	}
	blob, err := s.config.Store.GetPayloadSpans(threadID, payloadID)
	if err != nil {
		log.Printf("highlightapp: read payload spans %s: %v", payloadID, err)
		return nil
	}
	if blob == "" {
		return nil
	}
	var spans PersistedPatchSpans
	if err := json.Unmarshal([]byte(blob), &spans); err != nil {
		log.Printf("highlightapp: decode payload spans %s: %v", payloadID, err)
		return nil
	}
	if spans.Version != highlight.SchemaVersion() || len(spans.Files) == 0 {
		return nil
	}
	return spans.Files
}

func DiffPayloadKind(kind string) bool { return kind == "diff" || kind == "tool_result" }
