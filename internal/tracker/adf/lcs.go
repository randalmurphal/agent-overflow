package adf

import "context"

// Keep the dense table below roughly 8 MiB including its boundary row.
const alignmentCells = 1 << 20

// lcs returns each a index's matching b index or -1. Common ends keep the
// established matching policy. Large interiors use Hirschberg's algorithm:
// the same maximum reuse length with linear workspace and deterministic ties.
func lcs(ctx context.Context, a, b []string, cells int) ([]int, error) {
	match := make([]int, len(a))
	for i := range match {
		match[i] = -1
	}
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		match[pre] = pre
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		match[len(a)-1-suf] = len(b) - 1 - suf
		suf++
	}
	a, b = a[pre:len(a)-suf], b[pre:len(b)-suf]
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(a) == 0 || len(b) == 0 {
		return match, nil
	}
	if cells > 0 && len(a) <= cells/len(b) {
		return match, denseLCS(ctx, a, b, match[pre:], pre)
	}
	workspace := make([]int, 2*(len(b)+1))
	return match, linearLCS(ctx, a, b, match[pre:], pre, workspace)
}

func denseLCS(ctx context.Context, a, b []string, match []int, offset int) error {
	stride := len(b) + 1
	dp := make([]int32, (len(a)+1)*stride)
	for i := len(a) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, next := dp[i*stride:], dp[(i+1)*stride:]
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				row[j] = next[j+1] + 1
			} else {
				row[j] = max(next[j], row[j+1])
			}
		}
	}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			match[i] = offset + j
			i++
			j++
		case dp[(i+1)*stride+j] >= dp[i*stride+j+1]:
			i++
		default:
			j++
		}
	}
	return nil
}

func linearLCS(ctx context.Context, a, b []string, match []int, offset int, work []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	if len(a) == 1 {
		for j := range b {
			if a[0] == b[j] {
				match[0] = offset + j
				break
			}
		}
		return nil
	}
	mid := len(a) / 2
	k, err := lcsSplit(ctx, a, b, mid, work)
	if err != nil {
		return err
	}
	if err := linearLCS(ctx, a[:mid], b[:k], match[:mid], offset, work); err != nil {
		return err
	}
	return linearLCS(ctx, a[mid:], b[k:], match[mid:], offset+k, work)
}

func lcsSplit(ctx context.Context, a, b []string, mid int, work []int) (int, error) {
	left, right := work[:len(b)+1], work[len(b)+1:2*(len(b)+1)]
	clear(left)
	clear(right)
	for i := 0; i < mid; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		diagonal := 0
		for j := range b {
			old := left[j+1]
			if a[i] == b[j] {
				left[j+1] = diagonal + 1
			} else {
				left[j+1] = max(left[j], old)
			}
			diagonal = old
		}
	}
	for i := len(a) - 1; i >= mid; i-- {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		diagonal := 0
		for j := len(b) - 1; j >= 0; j-- {
			old := right[j]
			if a[i] == b[j] {
				right[j] = diagonal + 1
			} else {
				right[j] = max(right[j+1], old)
			}
			diagonal = old
		}
	}
	split, best := 0, -1
	for k := range left {
		if score := left[k] + right[k]; score > best {
			split, best = k, score
		}
	}
	return split, nil
}

// matchRuns picks, in order and without overlap, the runs (nil runs never
// match) to match against consecutive b keys so the most b keys are
// matched, and returns each run's first b index or -1. Matching ends are
// taken first: a run equal to the b keys at the current end is always
// part of some best answer. An interior larger than cells falls back to
// the per-key LCS, which keeps a run only when its keys line up.
func matchRuns(ctx context.Context, runs [][]string, b []string,
	cells int) ([]int, error) {
	start := make([]int, len(runs))
	for i := range start {
		start[i] = -1
	}
	at := func(i, j int) bool {
		r := runs[i]
		if r == nil || j+len(r) > len(b) {
			return false
		}
		for k, s := range r {
			if b[j+k] != s {
				return false
			}
		}
		return true
	}
	lo, j := 0, 0
	for ; lo < len(runs); lo++ {
		if runs[lo] == nil {
			continue
		}
		if !at(lo, j) {
			break
		}
		start[lo], j = j, j+len(runs[lo])
	}
	hi, e := len(runs), len(b)
	for hi > lo {
		r := runs[hi-1]
		if r == nil {
			hi--
			continue
		}
		if e-len(r) < j || !at(hi-1, e-len(r)) {
			break
		}
		e -= len(r)
		start[hi-1] = e
		hi--
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, m := hi-lo, e-j
	if n == 0 || m == 0 {
		return start, nil
	}
	if cells > 0 && n > cells/m {
		return start, flatRuns(ctx, runs[lo:hi], b[j:e], start[lo:hi], j)
	}
	stride := m + 1
	dp := make([]int32, (n+1)*stride)
	for i := n - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, next := dp[i*stride:], dp[(i+1)*stride:]
		for k := m - 1; k >= 0; k-- {
			best := max(next[k], row[k+1])
			if l := len(runs[lo+i]); l > 0 && k+l <= m && at(lo+i, j+k) {
				best = max(best, int32(l)+next[k+l])
			}
			row[k] = best
		}
	}
	for i, k := 0, 0; i < n && k < m; {
		l := len(runs[lo+i])
		switch {
		case l > 0 && k+l <= m && at(lo+i, j+k) &&
			dp[i*stride+k] == int32(l)+dp[(i+1)*stride+k+l]:
			start[lo+i] = j + k
			i++
			k += l
		case dp[i*stride+k] == dp[(i+1)*stride+k]:
			i++
		default:
			k++
		}
	}
	return start, nil
}

// flatRuns is the large-input fallback: a per-key LCS, keeping a run only
// when all its keys match consecutive b keys.
func flatRuns(ctx context.Context, runs [][]string, b []string, start []int,
	offset int) error {
	var a []string
	for _, r := range runs {
		a = append(a, r...)
	}
	match, err := lcs(ctx, a, b, 0)
	if err != nil {
		return err
	}
	pos := 0
	for i, r := range runs {
		ok := len(r) > 0
		for k := range r {
			m := match[pos+k]
			if m < 0 || (k > 0 && m != match[pos+k-1]+1) {
				ok = false
			}
		}
		if ok {
			start[i] = offset + match[pos]
		}
		pos += len(r)
	}
	return nil
}
