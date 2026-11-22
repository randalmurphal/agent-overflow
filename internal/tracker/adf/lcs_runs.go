package adf

import "context"

// matchRuns picks, in order and without overlap, the runs (nil runs never
// match) to match against consecutive b keys so the most b keys are
// matched, and returns each run's first b index or -1. An interior larger
// than cells is matched in linear space; both paths find a best answer.
//
// The runs matching at either end are taken first while that is provably
// part of a best answer. Let r, of length L, be the first run left and
// match b[j:j+L]. In a best answer that places r, moving r to j keeps it
// valid. In one that does not, the runs placed on b[j:j+L] all follow r;
// when none reaches past j+L they hold at most L keys, and placing r there
// instead loses none. A run that starts before j+L and reaches past it
// holds b[j+L-1] and b[j+L] as neighbors, so r is taken only when no run
// does. The last run at the far end is the mirror case.
func matchRuns(ctx context.Context, runs [][]string, b []string,
	cells int) ([]int, error) {
	start := make([]int, len(runs))
	for i := range start {
		start[i] = -1
	}
	m := &runMatcher{runs: runs, b: b}
	lo, j := 0, 0
	for ; lo < len(runs); lo++ {
		r := runs[lo]
		if r == nil {
			continue
		}
		if !m.at(lo, j, len(b)) || m.straddled(j+len(r), j, len(b)) {
			break
		}
		start[lo], j = j, j+len(r)
	}
	hi, e := len(runs), len(b)
	for ; hi > lo; hi-- {
		r := runs[hi-1]
		if r == nil {
			continue
		}
		s := e - len(r)
		if s < j || !m.at(hi-1, s, e) || m.straddled(s, j, e) {
			break
		}
		start[hi-1], e = s, s
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, w := hi-lo, e-j
	if n == 0 || w == 0 {
		return start, nil
	}
	if cells > 0 && n > cells/w {
		return start, m.linear(ctx, lo, hi, j, e, start, make([]int, 4*(w+1)))
	}
	return start, m.dense(ctx, lo, hi, j, e, start)
}

type runMatcher struct {
	runs [][]string
	b    []string
	// pairs holds every two keys that are neighbors in a run, built on
	// first use; indexed says it was.
	pairs   map[[2]string]bool
	indexed bool
}

// at reports whether run i matches b[s:] and ends by to.
func (m *runMatcher) at(i, s, to int) bool {
	r := m.runs[i]
	if r == nil || s+len(r) > to {
		return false
	}
	for k, key := range r {
		if m.b[s+k] != key {
			return false
		}
	}
	return true
}

// straddled reports whether some run holds b[p-1] and b[p] as neighbors,
// with both inside b[from:to], so it could match across p.
func (m *runMatcher) straddled(p, from, to int) bool {
	if p <= from || p >= to {
		return false
	}
	if !m.indexed {
		m.indexed = true
		n := 0
		for _, r := range m.runs {
			n += max(len(r)-1, 0)
		}
		if n > 0 {
			m.pairs = make(map[[2]string]bool, n)
		}
		for _, r := range m.runs {
			for k := 1; k < len(r); k++ {
				m.pairs[[2]string{r[k-1], r[k]}] = true
			}
		}
	}
	return m.pairs[[2]string{m.b[p-1], m.b[p]}]
}

// dense matches runs[lo:hi] against b[j:e] with the whole table.
func (m *runMatcher) dense(ctx context.Context, lo, hi, j, e int, start []int) error {
	n, w := hi-lo, e-j
	stride := w + 1
	dp := make([]int32, (n+1)*stride)
	for i := n - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, next := dp[i*stride:], dp[(i+1)*stride:]
		l := len(m.runs[lo+i])
		for k := w - 1; k >= 0; k-- {
			best := max(next[k], row[k+1])
			if m.at(lo+i, j+k, e) {
				best = max(best, int32(l)+next[k+l])
			}
			row[k] = best
		}
	}
	for i, k := 0, 0; i < n && k < w; {
		l := len(m.runs[lo+i])
		switch {
		case m.at(lo+i, j+k, e) && dp[i*stride+k] == int32(l)+dp[(i+1)*stride+k+l]:
			start[lo+i] = j + k
			i++
			k += l
		case dp[i*stride+k] == dp[(i+1)*stride+k]:
			i++
		default:
			k++
		}
	}
	return nil
}

// linear matches runs[lo:hi] against b[j:e] in Hirschberg's way: split the
// runs in half, find the b index a best answer splits at from the two
// halves' scores, and match each side. work holds four rows of e-j+1.
func (m *runMatcher) linear(ctx context.Context, lo, hi, j, e int, start, work []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lo == hi || j == e {
		return nil
	}
	if hi-lo == 1 {
		for s := j; s < e; s++ {
			if m.at(lo, s, e) {
				start[lo] = s
				break
			}
		}
		return nil
	}
	mid := (lo + hi) / 2
	k, err := m.split(ctx, lo, mid, hi, j, e, work)
	if err != nil {
		return err
	}
	if err := m.linear(ctx, lo, mid, j, k, start, work); err != nil {
		return err
	}
	return m.linear(ctx, mid, hi, k, e, start, work)
}

// split returns the b index in [j, e] that maximizes the best score of
// runs[lo:mid] before it plus that of runs[mid:hi] from it.
func (m *runMatcher) split(ctx context.Context, lo, mid, hi, j, e int, work []int) (int, error) {
	w := e - j
	fwd, row := work[:w+1], work[w+1:2*(w+1)]
	bwd, back := work[2*(w+1):3*(w+1)], work[3*(w+1):4*(w+1)]
	clear(fwd)
	clear(bwd)
	for i := lo; i < mid; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		l := len(m.runs[i])
		row[0] = fwd[0]
		for k := 1; k <= w; k++ {
			best := max(row[k-1], fwd[k])
			if k >= l && m.at(i, j+k-l, e) {
				best = max(best, fwd[k-l]+l)
			}
			row[k] = best
		}
		fwd, row = row, fwd
	}
	for i := hi - 1; i >= mid; i-- {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		l := len(m.runs[i])
		back[w] = bwd[w]
		for k := w - 1; k >= 0; k-- {
			best := max(back[k+1], bwd[k])
			if m.at(i, j+k, e) {
				best = max(best, bwd[k+l]+l)
			}
			back[k] = best
		}
		bwd, back = back, bwd
	}
	split, best := 0, -1
	for k := range fwd {
		if score := fwd[k] + bwd[k]; score > best {
			split, best = k, score
		}
	}
	return j + split, nil
}
