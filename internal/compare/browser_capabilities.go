package compare

import (
	"encoding/json"
	"fmt"
	"sort"

	"agent-overflow/internal/harnessclient"
)

func runnerCapabilities(c harnessclient.HarnessCapabilities, pageID string, cdp bool) []string {
	seen := map[string]bool{"browser": pageID != ""}
	if cdp {
		seen["cdp"] = true
	}
	for _, method := range c.Methods {
		seen["method:"+method] = true
	}
	for _, query := range c.Queries {
		seen["query:"+query] = true
	}
	capabilities := make([]string, 0, len(seen))
	for name, available := range seen {
		if available {
			capabilities = append(capabilities, name)
		}
	}
	sort.Strings(capabilities)
	return capabilities
}

func requireMethod(c harnessclient.HarnessCapabilities, name string) error {
	for _, got := range c.Methods {
		if got == name {
			return nil
		}
	}
	return fmt.Errorf("backend does not expose required method %q", name)
}

func checkRequiredCapabilities(c harnessclient.HarnessCapabilities, required []string, pageID string, cdp, replay bool, instrument string) error {
	available := map[string]bool{"browser": pageID != "", "cdp": cdp, "replay": replay}
	for _, method := range c.Methods {
		available["method:"+method] = true
	}
	for _, query := range c.Queries {
		available["query:"+query] = true
	}
	available["perf"] = instrument == "perf" && available["method:HarnessPerfStart"] && available["method:HarnessPerfStop"]
	for _, want := range required {
		if !available[want] {
			return fmt.Errorf("runner lacks required capability %q", want)
		}
	}
	return nil
}

func perfStartSpec(pageID string, o BrowserRunnerOptions) map[string]any {
	spec := map[string]any{"pageId": pageID}
	if o.SampleMs > 0 {
		spec["sampleMs"] = o.SampleMs
	}
	if len(o.PerfMeters) > 0 {
		spec["meters"] = o.PerfMeters
	}
	return spec
}

func metricsFromPerf(raw json.RawMessage) map[string]float64 {
	type series struct {
		Count int     `json:"count"`
		Max   float64 `json:"max"`
	}
	var doc struct {
		Samples  int `json:"samples"`
		Frontend *struct {
			Meters            []string `json:"meters"`
			UnavailableMeters []string `json:"unavailableMeters"`
			Frames            struct {
				FPS float64 `json:"fps"`
				P95 float64 `json:"p95Ms"`
				Max float64 `json:"maxMs"`
			} `json:"frames"`
			Busy struct {
				Ticks int     `json:"ticks"`
				P95   float64 `json:"p95Ms"`
				Max   float64 `json:"maxMs"`
			} `json:"busy"`
		} `json:"frontend"`
		Backend struct {
			RSS        series `json:"rssBytes"`
			Heap       series `json:"heapBytes"`
			Goroutines series `json:"goroutines"`
		} `json:"backend"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return map[string]float64{}
	}
	out := map[string]float64{}
	if doc.Backend.RSS.Count > 0 {
		out["backend.rssBytes"] = doc.Backend.RSS.Max
	}
	if doc.Backend.Heap.Count > 0 {
		out["backend.heapBytes"] = doc.Backend.Heap.Max
	}
	if doc.Backend.Goroutines.Count > 0 {
		out["backend.goroutines"] = doc.Backend.Goroutines.Max
	}
	if doc.Frontend != nil {
		if perfMeterMeasured(doc.Frontend.Meters, doc.Frontend.UnavailableMeters, "frames") {
			out["frames.fps"] = doc.Frontend.Frames.FPS
			out["frames.p95Ms"] = doc.Frontend.Frames.P95
			out["frames.maxMs"] = doc.Frontend.Frames.Max
		}
		if perfMeterMeasured(doc.Frontend.Meters, doc.Frontend.UnavailableMeters, "busy") && (doc.Frontend.Busy.Ticks > 0 || doc.Frontend.Meters == nil) {
			out["busy.p95Ms"] = doc.Frontend.Busy.P95
			out["busy.maxMs"] = doc.Frontend.Busy.Max
		}
	}
	return out
}

func perfMeterMeasured(selected, unavailable []string, meter string) bool {
	for _, name := range unavailable {
		if name == meter {
			return false
		}
	}
	if selected == nil {
		return true
	}
	for _, name := range selected {
		if name == meter {
			return true
		}
	}
	return false
}

func hasFrontendOnlyWork(w WorkloadShape) bool { return w.Name != "" }
