package trackerdash

import (
	"strings"
	"testing"
)

// A metric's series are each named, at most a title long, and named once.
func TestAMetricNamesEachSeriesOnce(t *testing.T) {
	t.Parallel()
	with := func(series ...string) Metric {
		m := createdFinishedMetric
		m.Series = series
		return m
	}
	if err := with("Created", "Finished").Validate(); err != nil {
		t.Fatalf("two series refused: %v", err)
	}
	for name, series := range map[string][]string{
		"named twice":        {"Created", "Created"},
		"named twice, apart": {"Created", "Finished", "Created"},
		"empty":              {"Created", " "},
		"over long":          {strings.Repeat("a", maxTitle+1)},
	} {
		if err := with(series...).Validate(); err == nil || !strings.Contains(err.Error(), "named twice") {
			t.Errorf("%s: %q validated as %v; want the series refused", name, series, err)
		}
	}
}
