package compare

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

func validateResult(c Capsule, opts RunOptions, r LegResult) error {
	if r.AssetDigest == "" {
		return fmt.Errorf("runner did not report an asset digest")
	}
	if r.BuildDigest == "" {
		return fmt.Errorf("runner did not report a build digest")
	}
	got := map[string]bool{}
	for _, cap := range r.Capabilities {
		got[cap] = true
	}
	for _, want := range c.Workload.RequiredCapabilities {
		if !got[want] {
			return fmt.Errorf("runner lacks required capability %q", want)
		}
	}
	if c.AssetDigest != "unknown" && r.AssetDigest != c.AssetDigest {
		return fmt.Errorf("asset digest mismatch: got %q, want %q", r.AssetDigest, c.AssetDigest)
	}
	if opts.ExpectedAsset != "" && r.AssetDigest != opts.ExpectedAsset {
		return fmt.Errorf("asset digest mismatch: got %q, want %q", r.AssetDigest, opts.ExpectedAsset)
	}
	if c.BuildDigest != "unknown" && r.BuildDigest != c.BuildDigest {
		return fmt.Errorf("build digest mismatch: got %q, want %q", r.BuildDigest, c.BuildDigest)
	}
	if opts.ExpectedBuild != "" && r.BuildDigest != opts.ExpectedBuild {
		return fmt.Errorf("build digest mismatch: got %q, want %q", r.BuildDigest, opts.ExpectedBuild)
	}
	for name, value := range r.Metrics {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("metric %q is not finite", name)
		}
	}
	return nil
}

func metricSetMismatch(a, b map[string]float64) string {
	missingFromB := make([]string, 0)
	missingFromA := make([]string, 0)
	for name := range a {
		if _, ok := b[name]; !ok {
			missingFromB = append(missingFromB, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			missingFromA = append(missingFromA, name)
		}
	}
	if len(missingFromA) == 0 && len(missingFromB) == 0 {
		return ""
	}
	sort.Strings(missingFromA)
	sort.Strings(missingFromB)
	return fmt.Sprintf("A/B metric sets differ (missing from A: %s; missing from B: %s)", strings.Join(missingFromA, ","), strings.Join(missingFromB, ","))
}

func identityMismatch(a, b LegReport) string {
	if a.AssetDigest != b.AssetDigest || a.BuildDigest != b.BuildDigest {
		return fmt.Sprintf("A/B identity differs (asset %q vs %q; build %q vs %q)", a.AssetDigest, b.AssetDigest, a.BuildDigest, b.BuildDigest)
	}
	return ""
}
