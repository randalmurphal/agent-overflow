package projectapp

import (
	"slices"

	"agent-overflow/internal/store"
)

// ThreadLockOrder returns a stable parent-before-descendant order compatible
// with recursive thread deletion's lock hierarchy.
func ThreadLockOrder(threads map[string]store.Thread) []string {
	children := make(map[string][]string, len(threads))
	var roots []string
	for id, thread := range threads {
		if _, parentInProject := threads[thread.ParentThreadID]; parentInProject {
			children[thread.ParentThreadID] = append(children[thread.ParentThreadID], id)
		} else {
			roots = append(roots, id)
		}
	}
	slices.Sort(roots)
	for parentID := range children {
		slices.Sort(children[parentID])
	}
	order := make([]string, 0, len(threads))
	visited := make(map[string]bool, len(threads))
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		order = append(order, id)
		for _, childID := range children[id] {
			visit(childID)
		}
	}
	for _, rootID := range roots {
		visit(rootID)
	}
	var leftovers []string
	for id := range threads {
		if !visited[id] {
			leftovers = append(leftovers, id)
		}
	}
	slices.Sort(leftovers)
	for _, id := range leftovers {
		visit(id)
	}
	return order
}
