package highlightapp

import (
	"encoding/json"
	"log"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
)

const (
	codeSpansMaxScanBytes      = 1 << 20
	codeSpansMaxSourceBytes    = 128 << 10
	persistedCodeSpansMaxBytes = 256 << 10
)

type PersistedCodeSpan struct {
	Lang       string                  `json:"lang"`
	ContentKey string                  `json:"contentKey"`
	Lines      []highlight.EncodedLine `json:"lines"`
}

type PersistedCodeSpans struct {
	Version string              `json:"hv"`
	Blocks  []PersistedCodeSpan `json:"blocks"`
}

func (s *Service) BuildPersistedCodeSpans(text string) json.RawMessage {
	if text == "" || len(text) > codeSpansMaxScanBytes {
		return nil
	}
	var blocks []PersistedCodeSpan
	budget := persistedCodeSpansMaxBytes
	for _, fence := range highlight.ScanFences(text) {
		if fence.Lang == "" || len(fence.Source) > codeSpansMaxSourceBytes || !utf8.ValidString(fence.Source) {
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
