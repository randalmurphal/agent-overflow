package adf

import "context"

// Keep the dense table below roughly 8 MiB including its boundary row.
const alignmentCells = 1 << 20

// lcs returns each a index's matching b index or -1, matching as many as
// any order-keeping matching can. Equal ends are matched first, which a
// best answer allows: when a[0] == b[0], a best answer matches at most one
// of them, to a later index, or neither, and pointing that match at the
// pair instead, or adding the pair, keeps its count; the last keys are the
// mirror case. Large interiors use Hirschberg's algorithm: the same count
// with linear workspace and deterministic ties.
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
