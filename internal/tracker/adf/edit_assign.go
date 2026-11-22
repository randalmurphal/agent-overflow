package adf

import (
	"slices"
	"strings"
)

// shaped is an unmatched container: its index, shape, child keys, gap,
// id (its key: two alike containers may stand for each other) and words.
type shaped struct {
	at    int
	shape string
	kids  map[string]int
	gap   int
	id    string
	words map[string]int
}

func newShaped(at int, shape string, n, kidsOf *Node, gap int) shaped {
	return shaped{at, shape, childKeys(kidsOf), gap, key(n), wordBag(n)}
}

// childKeys counts the keys of a container's blocks (a layout's, across
// its columns).
func childKeys(n *Node) map[string]int {
	keys := map[string]int{}
	for _, ch := range n.Content {
		if ch.Type == "layoutColumn" {
			for _, b := range ch.Content {
				keys[key(b)]++
			}
			continue
		}
		keys[key(ch)]++
	}
	return keys
}

func common(a, b map[string]int) int {
	n := 0
	for k, x := range a {
		n += min(x, b[k])
	}
	return n
}

// wordBag counts the words of n's text, which tells apart containers
// whose every child changed.
func wordBag(n *Node) map[string]int {
	bag := map[string]int{}
	walk(n, func(t, _ *Node) {
		for _, w := range strings.Fields(t.Text) {
			bag[w]++
		}
	})
	return bag
}

// assign pairs indexes of a and b with equal shapes by the children
// they share, the most first, then by the words they share; containers in
// different gaps pair only when they share children. It never guesses:
// when one container scores the same with two that differ, it pairs none
// of them, and their losses are refused; tied lists those of a. Pairs
// come back sorted by a.
func assign(a, b []shaped) (pairs [][2]int, tied []int) {
	type cand struct{ i, j, common, words int }
	var cs []cand
	for i := range a {
		for j := range b {
			if a[i].shape != b[j].shape {
				continue
			}
			c := common(a[i].kids, b[j].kids)
			if a[i].gap != b[j].gap && c == 0 {
				continue
			}
			cs = append(cs, cand{i, j, c, common(a[i].words, b[j].words)})
		}
	}
	slices.SortStableFunc(cs, func(x, y cand) int {
		if x.common != y.common {
			return y.common - x.common
		}
		return y.words - x.words
	})
	usedA, usedB := make([]bool, len(a)), make([]bool, len(b))
	var out [][2]int
	for lo := 0; lo < len(cs); {
		hi := lo
		for hi < len(cs) && cs[hi].common == cs[lo].common && cs[hi].words == cs[lo].words {
			hi++
		}
		var open []cand
		for _, c := range cs[lo:hi] {
			if !usedA[c.i] && !usedB[c.j] {
				open = append(open, c)
			}
		}
		var take, block []cand
		for _, c := range open {
			tied := false
			for _, d := range open {
				if d.i == c.i && d.j != c.j && b[d.j].id != b[c.j].id ||
					d.j == c.j && d.i != c.i && a[d.i].id != a[c.i].id {
					tied = true
					break
				}
			}
			if tied {
				block = append(block, c)
			} else {
				take = append(take, c)
			}
		}
		for _, c := range take {
			if !usedA[c.i] && !usedB[c.j] {
				usedA[c.i], usedB[c.j] = true, true
				out = append(out, [2]int{c.i, c.j})
			}
		}
		for _, c := range block {
			if !usedA[c.i] {
				tied = append(tied, c.i)
			}
			usedA[c.i], usedB[c.j] = true, true
		}
		lo = hi
	}
	slices.SortFunc(out, func(x, y [2]int) int { return x[0] - y[0] })
	return out, tied
}
