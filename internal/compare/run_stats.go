package compare

import (
	"math"
	"math/rand"
	"sort"
)

func pairedDeltas(a, b map[string]float64) map[string]float64 {
	out := map[string]float64{}
	keys := make([]string, 0, len(a))
	for k := range a {
		if _, ok := b[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = b[k] - a[k]
	}
	return out
}

func bootstrapIntervals(pairs []PairReport, capsuleSHA string, samples int) []ConfidenceInterval {
	if len(pairs) < BootstrapMinPairs || samples < 1 {
		return nil
	}
	seed := uint64(0)
	for _, b := range []byte(capsuleSHA) {
		seed = seed*131 + uint64(b)
	}
	keys := map[string]bool{}
	for _, p := range pairs {
		for k := range p.Deltas {
			keys[k] = true
		}
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]ConfidenceInterval, 0, len(names))
	for _, name := range names {
		values := make([]float64, 0, len(pairs))
		for _, p := range pairs {
			if v, ok := p.Deltas[name]; ok {
				values = append(values, v)
			}
		}
		if len(values) < BootstrapMinPairs {
			continue
		}
		rng := rand.New(rand.NewSource(int64(seed)))
		dist := make([]float64, samples)
		for i := range dist {
			sum := 0.0
			for j := 0; j < len(values); j++ {
				sum += values[rng.Intn(len(values))]
			}
			dist[i] = sum / float64(len(values))
		}
		sort.Float64s(dist)
		out = append(out, ConfidenceInterval{Metric: name, Pairs: len(values), Lower: quantile(dist, .025), Upper: quantile(dist, .975), Seed: seed, Samples: samples})
	}
	return out
}

func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Floor(p * float64(len(sorted)-1)))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
