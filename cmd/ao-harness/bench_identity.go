package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/harnessclient"
)

func ensureBenchBaselineMatches(current benchReportIdentity, baseline benchBaseline) error {
	if err := validateBenchBaselineIdentity(baseline); err != nil {
		return err
	}
	for _, item := range [][3]string{
		{"leg", current.Leg, baseline.Leg},
		{"instrument", current.Instrument, baseline.Instrument},
		{"pageId", current.PageID, baseline.PageID},
		{"buildFingerprint", current.BuildFingerprint, baseline.BuildFingerprint},
		{"assetsFingerprint", current.AssetsFingerprint, baseline.AssetsFingerprint},
		{"timingFingerprint", current.TimingFingerprint, baseline.TimingFingerprint},
		{"budgetFingerprint", current.BudgetFingerprint, baseline.BudgetFingerprint},
		{"monitorFingerprint", current.MonitorFingerprint, baseline.MonitorFingerprint},
		{"traceFingerprint", current.TraceFingerprint, baseline.TraceFingerprint},
		{"workloadFingerprint", current.WorkloadFingerprint, baseline.WorkloadFingerprint},
	} {
		if item[1] != item[2] {
			return fmt.Errorf("baseline %s %q is incompatible with current %q", item[0], item[2], item[1])
		}
	}
	return nil
}

func benchInstrumentName(perf benchPerfSpec, tracing bool) string {
	if tracing {
		return "trace"
	}
	if perf.Meters != nil && len(perf.Meters) == 0 {
		return "none"
	}
	if len(perf.Meters) == 0 {
		return "perf:all"
	}
	meters := append([]string(nil), perf.Meters...)
	sort.Strings(meters)
	return "perf:" + strings.Join(meters, ",")
}

type benchTimingIdentity struct {
	SampleMs          int   `json:"sampleMs"`
	RequestedDuration int64 `json:"requestedDurationMs"`
}

type benchBudgetIdentity struct {
	BudgetsMs []float64 `json:"budgetsMs"`
}

type benchMonitorIdentity struct {
	IDs []string `json:"ids"`
	Leg string   `json:"leg"`
}

type benchTraceIdentity struct {
	Enabled bool `json:"enabled"`
}

// benchReportIdentityFor includes every input that can change the meaning of
// a reported number. Baselines must refuse when any of these change, rather
// than comparing compatible-looking JSON from a different experiment.
func benchReportIdentityFor(
	bs harnessclient.Bootstrap,
	caps harnessclient.HarnessCapabilities,
	info harnessclient.HarnessInfo,
	workload benchWorkload,
	perf benchPerfSpec,
	duration time.Duration,
	tracing bool,
	leg string,
) benchReportIdentity {
	build := caps.Build.Stamp
	if strings.TrimSpace(build) == "" {
		build = bs.Version
	}
	meters := append([]string(nil), perf.Meters...)
	if meters == nil {
		meters = []string{"<all>"}
	}
	sort.Strings(meters)
	meters = slices.Compact(meters)
	monitorIDs := append([]string(nil), perf.Monitors...)
	sort.Strings(monitorIDs)
	monitorIDs = slices.Compact(monitorIDs)
	budgets := append([]float64(nil), perf.BudgetsMs...)
	sort.Float64s(budgets)
	budgets = slices.Compact(budgets)
	return benchReportIdentity{
		SchemaVersion:      benchReportSchemaVersion,
		Leg:                leg,
		Instrument:         benchInstrumentName(perf, tracing),
		PageID:             perf.PageID,
		BuildFingerprint:   fingerprintBenchParts(build, bs.Version),
		AssetsFingerprint:  fingerprintBenchParts(info.AssetsFreshness, info.AssetsDigest, caps.Assets.Freshness, caps.Assets.Digest),
		TimingFingerprint:  fingerprintJSON(benchTimingIdentity{SampleMs: resolvedBenchSampleMs(perf.SampleMs), RequestedDuration: duration.Milliseconds()}),
		BudgetFingerprint:  fingerprintJSON(benchBudgetIdentity{BudgetsMs: budgets}),
		MonitorFingerprint: fingerprintJSON(benchMonitorIdentity{IDs: monitorIDs, Leg: perf.MonitorLeg}),
		TraceFingerprint:   fingerprintJSON(benchTraceIdentity{Enabled: tracing}),
		WorkloadFingerprint: fingerprintJSON(struct {
			Name, Scenario, Summary string
			DefaultDurationMs       int64
			MinimumDurationMs       int64
		}{workload.Name, workload.Scenario, workload.Summary, workload.DefaultDuration.Milliseconds(), workload.MinimumDuration.Milliseconds()}),
	}
}

func fingerprintJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode benchmark identity: %v", err))
	}
	return fingerprintBenchParts(string(data))
}

func resolvedBenchSampleMs(requested int) int {
	if requested <= 0 {
		return harnessPerfDefaultSampleMs
	}
	if requested < harnessPerfMinSampleMs {
		return harnessPerfMinSampleMs
	}
	return requested
}

// executeBenchRun is one repeat: blank slate, fixture, armed meters, the
// workload, the report.
func readBenchBaseline(path string) (benchBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return benchBaseline{}, err
	}
	var baseline benchBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return benchBaseline{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	if err := validateBenchBaselineIdentity(baseline); err != nil {
		return benchBaseline{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	if len(baseline.Metrics) == 0 && len(baseline.Aggregate) == 0 {
		return benchBaseline{}, fmt.Errorf(
			"baseline %s carries neither `metrics` (a budget) nor `aggregate` (a previous bench report)", path)
	}
	return baseline, nil
}

func validateBenchBaselineIdentity(baseline benchBaseline) error {
	if baseline.SchemaVersion != benchReportSchemaVersion {
		if baseline.SchemaVersion == 0 {
			return fmt.Errorf("legacy baseline has no schemaVersion; regenerate it with this harness")
		}
		return fmt.Errorf("baseline schemaVersion %d is incompatible with %d", baseline.SchemaVersion, benchReportSchemaVersion)
	}
	for _, item := range [][2]string{
		{"leg", baseline.Leg},
		{"instrument", baseline.Instrument},
		{"buildFingerprint", baseline.BuildFingerprint},
		{"assetsFingerprint", baseline.AssetsFingerprint},
		{"timingFingerprint", baseline.TimingFingerprint},
		{"budgetFingerprint", baseline.BudgetFingerprint},
		{"monitorFingerprint", baseline.MonitorFingerprint},
		{"traceFingerprint", baseline.TraceFingerprint},
		{"workloadFingerprint", baseline.WorkloadFingerprint},
	} {
		if strings.TrimSpace(item[1]) == "" {
			return fmt.Errorf("baseline is missing %s", item[0])
		}
	}
	return nil
}
