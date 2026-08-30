// Package compare contains the offline capsule and A/B comparison engine.
// It deliberately does not import the app or the harness server. A browser
// driver is supplied by the caller, which keeps this package testable and
// makes an unavailable driver a visible failure rather than a fake result.
package compare

import (
	"context"
	"encoding/json"
	"time"
)

const (
	// CurrentVersion is the on-disk capsule and report schema version.
	CurrentVersion = 1
	// BootstrapMinPairs is the minimum number of complete paired observations
	// before a bootstrap interval is useful. Smaller samples still report the
	// paired deltas, but do not manufacture confidence.
	BootstrapMinPairs         = 8
	DefaultBootstrapResamples = 10_000
)

type Asset struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type LogicalPane struct {
	PaneID       string  `json:"paneId"`
	Kind         string  `json:"kind"`
	ThreadID     string  `json:"threadId,omitempty"`
	SourcePaneID string  `json:"sourcePaneId,omitempty"`
	WidthPX      float64 `json:"widthPx,omitempty"`
}

type WorkloadShape struct {
	Name                 string         `json:"name"`
	Parameters           map[string]any `json:"parameters,omitempty"`
	RequiredCapabilities []string       `json:"requiredCapabilities,omitempty"`
}

type Event struct {
	Ordinal   int             `json:"ordinal"`
	Timestamp int64           `json:"ts"`
	ThreadID  string          `json:"threadId"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
	SHA256    string          `json:"sha256"`
}

type EventStream struct {
	Path   string  `json:"path"`
	Count  int     `json:"count"`
	SHA256 string  `json:"sha256"`
	Events []Event `json:"events"`
}

type Provenance struct {
	Kind      string `json:"kind"`
	SourceSHA string `json:"sourceSha256,omitempty"`
	Source    string `json:"source,omitempty"`
}

type Capsule struct {
	Version       int           `json:"version"`
	CreatedAt     time.Time     `json:"createdAt"`
	Source        Provenance    `json:"source"`
	Database      Asset         `json:"database"`
	Attachments   []Asset       `json:"attachments,omitempty"`
	Fixtures      []Asset       `json:"fixtures,omitempty"`
	Panes         []LogicalPane `json:"panes,omitempty"`
	Events        EventStream   `json:"events"`
	AssetDigest   string        `json:"assetDigest"`
	BuildDigest   string        `json:"buildDigest"`
	Workload      WorkloadShape `json:"workload"`
	CapsuleSHA256 string        `json:"capsuleSha256"`
	manifestPath  string        `json:"-"`
}

type PrepareOptions struct {
	Source      string
	Output      string
	Force       bool
	AssetDigest string
	BuildDigest string
	Workload    WorkloadShape
}

type Leg string

const (
	LegA Leg = "A"
	LegB Leg = "B"
)

type LegRequest struct {
	Leg             Leg
	Pair            int
	Root            string
	CapsulePath     string
	DataDir         string
	Database        string
	BrowserProfile  string
	AttachmentsDir  string
	FixturesDir     string
	EventStreamPath string
	EventCount      int
	KeepRoot        bool
	Panes           []LogicalPane
	Workload        WorkloadShape
	// Instrument selects the observer for this leg. "perf" arms the
	// harness's frontend/backend sampler. "none" keeps the leg clean for
	// memory or correctness measurements.
	Instrument string
	// No seed or ID map is part of this request. The runner must consume the
	// restored database and preserve its IDs exactly as supplied.
}

type LegResult struct {
	Metrics            map[string]float64 `json:"metrics,omitempty"`
	SemanticText       string             `json:"-"`
	SemanticDigest     string             `json:"semanticDigest,omitempty"`
	AssetDigest        string             `json:"assetDigest,omitempty"`
	BuildDigest        string             `json:"buildDigest,omitempty"`
	Capabilities       []string           `json:"capabilities,omitempty"`
	SupervisorManifest string             `json:"supervisorManifest,omitempty"`
	PageID             string             `json:"pageId,omitempty"`
	CDPTargetID        string             `json:"cdpTargetId,omitempty"`
}

type LegReport struct {
	Leg                Leg                `json:"leg"`
	Pair               int                `json:"pair"`
	Status             string             `json:"status"`
	Instrument         string             `json:"instrument,omitempty"`
	Root               string             `json:"root,omitempty"`
	BrowserProfile     string             `json:"browserProfile,omitempty"`
	Metrics            map[string]float64 `json:"metrics,omitempty"`
	SemanticDigest     string             `json:"semanticDigest,omitempty"`
	AssetDigest        string             `json:"assetDigest,omitempty"`
	BuildDigest        string             `json:"buildDigest,omitempty"`
	SupervisorManifest string             `json:"supervisorManifest,omitempty"`
	PageID             string             `json:"pageId,omitempty"`
	CDPTargetID        string             `json:"cdpTargetId,omitempty"`
	Error              string             `json:"error,omitempty"`
}

type TextDifference struct {
	OffsetA int    `json:"offsetA"`
	OffsetB int    `json:"offsetB"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	A       string `json:"a,omitempty"`
	B       string `json:"b,omitempty"`
}

type TextGate struct {
	Equal           bool            `json:"equal"`
	ComparedPairs   int             `json:"comparedPairs"`
	DigestA         string          `json:"digestA,omitempty"`
	DigestB         string          `json:"digestB,omitempty"`
	FirstDifference *TextDifference `json:"firstDifference,omitempty"`
}

type PairReport struct {
	Pair   int                `json:"pair"`
	Deltas map[string]float64 `json:"deltas,omitempty"`
	Text   TextGate           `json:"text"`
}

type ConfidenceInterval struct {
	Metric  string  `json:"metric"`
	Pairs   int     `json:"pairs"`
	Lower   float64 `json:"lower"`
	Upper   float64 `json:"upper"`
	Seed    uint64  `json:"seed"`
	Samples int     `json:"samples"`
}

type Report struct {
	Version       int                  `json:"version"`
	StartedAt     time.Time            `json:"startedAt"`
	FinishedAt    *time.Time           `json:"finishedAt,omitempty"`
	Capsule       string               `json:"capsule"`
	CapsuleSHA256 string               `json:"capsuleSha256"`
	Legs          []LegReport          `json:"legs"`
	Pairs         []PairReport         `json:"pairs,omitempty"`
	Semantic      TextGate             `json:"semanticText"`
	Bootstrap     []ConfidenceInterval `json:"bootstrap,omitempty"`
	Complete      bool                 `json:"complete"`
	Error         string               `json:"error,omitempty"`
}

type Runner interface {
	Run(ctx context.Context, request LegRequest) (LegResult, error)
}
