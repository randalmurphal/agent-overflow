package trackerdash

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Num is one number a widget shows: a value, none (nothing to reduce:
// the median of no rows), not known (no row counted and NC rows not
// counted), or a gap (the mirror cannot answer: a previous range it holds
// too few sprints for). NC is the tickets the metric could not count
// beside the value. On the wire: {"v":3}, {"v":3,"nc":2}, {}, {"nc":2}
// or {"gap":true}.
type Num struct {
	V   *float64 `json:"v,omitempty"`
	NC  int      `json:"nc,omitempty"`
	Gap bool     `json:"gap,omitempty"`
}

func value(v float64) Num { return Num{V: &v} }

// notCounted is the tickets of rows that are not counted and that no
// counted row of rows counts.
func notCounted(rows []contribution) int {
	if !slices.ContainsFunc(rows, func(c contribution) bool { return !c.counted }) {
		return 0
	}
	seen := map[string]bool{}
	for _, c := range rows {
		if c.counted {
			seen[c.ticket] = true
		}
	}
	n := 0
	for _, c := range rows {
		if !c.counted && !seen[c.ticket] {
			seen[c.ticket] = true
			n++
		}
	}
	return n
}

var gapNum = Num{Gap: true}

// MarshalJSON writes a Num as its wire form; a gap carries no value.
func (n Num) MarshalJSON() ([]byte, error) {
	type wire Num
	if n.Gap {
		return []byte(`{"gap":true}`), nil
	}
	return json.Marshal(wire(n))
}

// reduceRows is the metric's reduction of rows within one cell: a bar, a
// table cell or a period, with the tickets it could not count. A sum of
// periods is a sum within one. When no row is counted and some are not,
// the number is not known: a zero would say what the rows do not.
func reduceRows(r Reducer, rows []contribution) Num {
	n := reduceCounted(r, rows)
	n.NC = notCounted(rows)
	if n.NC > 0 && !slices.ContainsFunc(rows, func(c contribution) bool { return c.counted }) {
		n.V = nil
	}
	return n
}

func reduceCounted(r Reducer, rows []contribution) Num {
	var values []float64
	tickets := map[string]bool{}
	for _, c := range rows {
		if c.counted {
			values = append(values, c.value)
			tickets[c.ticket] = true
		}
	}
	switch r {
	case ReduceCount:
		return value(float64(len(tickets)))
	case ReduceSum, ReduceAverageOfPeriodSums:
		total := 0.0
		for _, v := range values {
			total += v
		}
		return value(total)
	case ReduceAverage:
		if len(values) == 0 {
			return Num{}
		}
		total := 0.0
		for _, v := range values {
			total += v
		}
		return value(total / float64(len(values)))
	case ReduceMedian:
		if len(values) == 0 {
			return Num{}
		}
		slices.Sort(values)
		mid := len(values) / 2
		if len(values)%2 == 1 {
			return value(values[mid])
		}
		return value((values[mid-1] + values[mid]) / 2)
	}
	panic(fmt.Sprintf("trackerdash: reducer %q reached a reduction without validation", r))
}

// reduceRange is the metric's number over a range's periods and the
// range's unplaced rows (not counted, in no period). An average of period
// sums averages over the periods, a period with no counted rows as zero;
// any other reducer reduces the range's rows as one. A range of no
// periods has none.
func reduceRange(r Reducer, periods []period, placed map[string][]contribution, unplaced []contribution) Num {
	if len(periods) == 0 {
		return Num{}
	}
	rows := slices.Clone(unplaced)
	for _, p := range periods {
		rows = append(rows, placed[p.id]...)
	}
	n := reduceRows(r, rows)
	if r == ReduceAverageOfPeriodSums && n.V != nil {
		*n.V /= float64(len(periods))
	}
	return n
}

// place sorts a period-bound metric's rows into the periods of cur and
// prev by period ID: by the sprint its bucket names for a metric grouped
// by sprint, else by its at. A row that falls in no period is out of the
// range. A row with no at belongs to no period: it is unplaced, and not
// counted whatever its value, since the metric cannot say when it
// happened.
func place(m Metric, rows []contribution, periods ...[]period) (map[string][]contribution, []contribution) {
	out := map[string][]contribution{}
	var unplaced []contribution
	bySprint := map[string]period{}
	var all []period
	for _, ps := range periods {
		for _, p := range ps {
			all = append(all, p)
			for _, s := range p.sprints {
				bySprint[s] = p
			}
		}
	}
	for _, c := range rows {
		if m.Bucket == BucketSprint {
			if p, ok := bySprint[c.bucket]; ok {
				c.period = p.label
				out[p.id] = append(out[p.id], c)
			}
			continue
		}
		if !c.hasAt {
			c.counted = false
			unplaced = append(unplaced, c)
			continue
		}
		for _, p := range all {
			if p.holds(c.at) {
				c.period = p.label
				out[p.id] = append(out[p.id], c)
				break
			}
		}
	}
	return out, unplaced
}

// seriesOf is the series a widget's cells follow: the metric's declared
// series, which the sink holds every row to, or one unnamed series for a
// metric that names none.
func seriesOf(m Metric, cols columns) []string {
	if !cols.has(ColumnSeries) {
		return []string{""}
	}
	return slices.Clone(m.Series)
}

// group is rows by bucket key, the keys in first-seen order.
func group(rows []contribution, key func(contribution) string) ([]string, map[string][]contribution) {
	var keys []string
	out := map[string][]contribution{}
	for _, c := range rows {
		k := key(c)
		if _, ok := out[k]; !ok {
			keys = append(keys, k)
		}
		out[k] = append(out[k], c)
	}
	return keys, out
}

// bySeries is the rows of series s; all of them for the unnamed one.
func bySeries(rows []contribution, s string, named bool) []contribution {
	if !named {
		return rows
	}
	var out []contribution
	for _, c := range rows {
		if c.series == s {
			out = append(out, c)
		}
	}
	return out
}

// sortDrill orders a cell's rows as the drill and the ticket list show
// them: counted rows by value, largest first, then by ticket key; then the
// rows the metric does not count, by key.
func sortDrill(rows []contribution, keyOf func(string) string) {
	slices.SortStableFunc(rows, func(a, b contribution) int {
		if a.counted != b.counted {
			if a.counted {
				return -1
			}
			return 1
		}
		if a.counted && a.value != b.value {
			return cmp.Compare(b.value, a.value)
		}
		return compareKeys(keyOf(a.ticket), keyOf(b.ticket))
	})
}

// compareKeys orders ticket keys as people read them: WEB-9 before WEB-10.
func compareKeys(a, b string) int {
	pa, na, oka := splitKey(a)
	pb, nb, okb := splitKey(b)
	if oka && okb && pa == pb {
		return cmp.Compare(na, nb)
	}
	return strings.Compare(a, b)
}

func splitKey(k string) (string, int, bool) {
	i := strings.LastIndexByte(k, '-')
	if i < 0 {
		return "", 0, false
	}
	n := 0
	for _, r := range k[i+1:] {
		if r < '0' || r > '9' || n > 1e9 {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	return k[:i], n, i+1 < len(k)
}
