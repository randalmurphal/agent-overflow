package compare

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type RunOptions struct {
	Capsule       string
	BaseDir       string
	Pairs         int
	Bootstrap     int
	KeepRoots     bool
	ReportPath    string
	ExpectedAsset string
	ExpectedBuild string
	// Instrument is passed to every leg. Empty means "perf", which keeps the
	// command observable by default. Use "none" for a clean leg.
	Instrument string
}

// UnavailableRunner is the default command runner. A browser adapter cannot
// be inferred from a filesystem capsule, so this error is intentional and
// explicit. Tests and integrations provide a real Runner implementation.
type UnavailableRunner struct{}

func (UnavailableRunner) Run(context.Context, LegRequest) (LegResult, error) {
	return LegResult{}, errors.New("compare run has no browser runner; provide an integration runner")
}

func Run(ctx context.Context, opts RunOptions, runner Runner, progress func(Report) error) (Report, error) {
	if runner == nil {
		runner = UnavailableRunner{}
	}
	if opts.Pairs < 1 {
		opts.Pairs = 1
	}
	if opts.Bootstrap == 0 {
		opts.Bootstrap = DefaultBootstrapResamples
	}
	if opts.Instrument == "" {
		opts.Instrument = "perf"
	}
	if opts.Instrument != "perf" && opts.Instrument != "none" {
		return Report{}, fmt.Errorf("compare instrument %q is unsupported (want perf or none)", opts.Instrument)
	}
	capsulePath, err := filepath.Abs(opts.Capsule)
	if err != nil {
		return Report{}, fmt.Errorf("resolve capsule path: %w", err)
	}
	baseDir, reportPath, err := validateRunPaths(capsulePath, opts.BaseDir, opts.ReportPath)
	if err != nil {
		return Report{}, err
	}
	opts.BaseDir = baseDir
	opts.ReportPath = reportPath
	capsule, err := Load(capsulePath)
	if err != nil {
		return Report{}, err
	}
	if opts.ReportPath != "" {
		reportPath, pathErr := filepath.Abs(opts.ReportPath)
		if pathErr != nil {
			return Report{}, fmt.Errorf("resolve report path: %w", pathErr)
		}
		if samePathOrAncestor(reportPath, filepath.Dir(cPath(capsule))) {
			return Report{}, fmt.Errorf("compare report %s is inside immutable capsule %s", reportPath, filepath.Dir(cPath(capsule)))
		}
	}
	report := Report{Version: CurrentVersion, StartedAt: time.Now().UTC(), Capsule: capsulePath, CapsuleSHA256: capsule.CapsuleSHA256, Legs: make([]LegReport, 0, opts.Pairs*2)}
	writePartial := func() error {
		if progress != nil {
			return progress(report)
		}
		if opts.ReportPath != "" {
			return writeReport(opts.ReportPath, report)
		}
		return nil
	}
	var runErrs []string
	for pair := 1; pair <= opts.Pairs; pair++ {
		var pairRuns [2]*LegReport
		var pairTexts [2]string
		for _, leg := range []Leg{LegA, LegB} {
			root, rootIdentity, err := materialize(capsule, opts.BaseDir, string(leg), pair)
			legReport := LegReport{Leg: leg, Pair: pair, Status: "refused", Instrument: opts.Instrument}
			if err == nil {
				legReport.Root = root
				legReport.BrowserProfile = filepath.Join(root, "browser")
				request := LegRequest{Leg: leg, Pair: pair, Root: root, CapsulePath: capsulePath, DataDir: filepath.Join(root, "agent-overflow"), Database: filepath.Join(root, "agent-overflow", "agent-overflow.db"), BrowserProfile: legReport.BrowserProfile, AttachmentsDir: filepath.Join(root, "agent-overflow", "attachments"), FixturesDir: filepath.Join(root, "agent-overflow", "fixtures"), EventStreamPath: filepath.Join(root, "agent-overflow", "events.jsonl"), EventCount: capsule.Events.Count, KeepRoot: opts.KeepRoots, Panes: capsule.Panes, Workload: capsule.Workload}
				request.Instrument = opts.Instrument
				result, runErr := runner.Run(ctx, request)
				legReport.AssetDigest = result.AssetDigest
				legReport.BuildDigest = result.BuildDigest
				if runErr == nil {
					runErr = validateResult(capsule, opts, result)
				}
				if runErr == nil {
					legReport.Status = "ok"
					legReport.Metrics = cloneMetrics(result.Metrics)
					legReport.SemanticDigest = semanticDigest(result.SemanticText)
					legReport.SupervisorManifest = result.SupervisorManifest
					legReport.PageID = result.PageID
					legReport.CDPTargetID = result.CDPTargetID
					pairTexts[legIndex(leg)] = result.SemanticText
				} else {
					legReport.Error = runErr.Error()
					legReport.SupervisorManifest = result.SupervisorManifest
					runErrs = append(runErrs, fmt.Sprintf("%s%d: %v", leg, pair, runErr))
				}
			} else {
				legReport.Error = err.Error()
				runErrs = append(runErrs, fmt.Sprintf("%s%d: %v", leg, pair, err))
			}
			report.Legs = append(report.Legs, legReport)
			pairRuns[legIndex(leg)] = &report.Legs[len(report.Legs)-1]
			if !opts.KeepRoots && opts.ReportPath != "" {
				resultManifest := report.Legs[len(report.Legs)-1].SupervisorManifest
				if resultManifest != "" {
					retained := filepath.Join(filepath.Dir(opts.ReportPath), fmt.Sprintf("compare-supervisor-%s%d.json", leg, pair))
					if copyErr := copyOne(resultManifest, retained, 0o600); copyErr != nil {
						runErrs = append(runErrs, fmt.Sprintf("retain supervisor manifest for %s%d: %v", leg, pair, copyErr))
					} else {
						report.Legs[len(report.Legs)-1].SupervisorManifest = retained
					}
				}
			}
			if !opts.KeepRoots && root != "" && !(legReport.Status != "ok" && legReport.SupervisorManifest != "") {
				if removeErr := removeDisposableRoot(root, rootIdentity); removeErr != nil {
					runErrs = append(runErrs, fmt.Sprintf("remove compare root %s: %v", root, removeErr))
				}
			}
			// The B-leg receipt is written below after its pair is folded, so
			// a crash after B1 still leaves the complete A1/B1 comparison.
			if leg == LegA {
				if err := writePartial(); err != nil {
					runErrs = append(runErrs, fmt.Sprintf("write partial report: %v", err))
				}
			}
		}
		if pairRuns[0].Status == "ok" && pairRuns[1].Status == "ok" {
			if mismatch := identityMismatch(*pairRuns[0], *pairRuns[1]); mismatch != "" {
				pairRuns[0].Status = "invalid"
				pairRuns[1].Status = "invalid"
				pairRuns[0].Error = mismatch
				pairRuns[1].Error = mismatch
				runErrs = append(runErrs, fmt.Sprintf("pair %d: %s", pair, mismatch))
			} else if mismatch := metricSetMismatch(pairRuns[0].Metrics, pairRuns[1].Metrics); mismatch != "" {
				pairRuns[0].Status = "invalid"
				pairRuns[1].Status = "invalid"
				pairRuns[0].Error = mismatch
				pairRuns[1].Error = mismatch
				runErrs = append(runErrs, fmt.Sprintf("pair %d: %s", pair, mismatch))
			} else {
				deltas := pairedDeltas(pairRuns[0].Metrics, pairRuns[1].Metrics)
				text := CompareText(pairTexts[0], pairTexts[1])
				text.ComparedPairs = 1
				report.Pairs = append(report.Pairs, PairReport{Pair: pair, Deltas: deltas, Text: text})
				if report.Semantic.ComparedPairs == 0 {
					report.Semantic = text
				} else {
					report.Semantic.ComparedPairs++
					if report.Semantic.FirstDifference == nil && text.FirstDifference != nil {
						report.Semantic.Equal = false
						report.Semantic.FirstDifference = text.FirstDifference
					}
				}
				if !text.Equal {
					pairRuns[0].Status = "invalid"
					pairRuns[1].Status = "invalid"
					pairRuns[0].Error = "semantic text mismatch"
					pairRuns[1].Error = "semantic text mismatch"
					runErrs = append(runErrs, fmt.Sprintf("pair %d: semantic text mismatch", pair))
				}
			}
		}
		if err := writePartial(); err != nil {
			runErrs = append(runErrs, fmt.Sprintf("write partial report: %v", err))
		}
	}
	report.Bootstrap = bootstrapIntervals(report.Pairs, capsule.CapsuleSHA256, opts.Bootstrap)
	report.Complete = len(runErrs) == 0
	if len(runErrs) > 0 {
		report.Error = strings.Join(runErrs, "; ")
	}
	now := time.Now().UTC()
	report.FinishedAt = &now
	if err := writePartial(); err != nil {
		runErrs = append(runErrs, fmt.Sprintf("write final report: %v", err))
		report.Complete = false
		report.Error = strings.Join(runErrs, "; ")
	}
	if len(runErrs) > 0 {
		return report, errors.New(strings.Join(runErrs, "; "))
	}
	return report, nil
}

func WriteReport(path string, report Report) error {
	return writeReport(path, report)
}
