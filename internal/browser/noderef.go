package browser

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const maxNodeReferences = 2000

type nodeReference struct {
	Selector string
	Frames   []string
	Tag      string
	Text     string
}

func (p *managedPage) rememberNode(ref nodeReference) string {
	id := uuid.NewString()
	p.nodeMu.Lock()
	defer p.nodeMu.Unlock()
	p.nodeRefs[id] = ref
	p.nodeOrder = append(p.nodeOrder, id)
	if len(p.nodeOrder) > maxNodeReferences {
		drop := p.nodeOrder[0]
		p.nodeOrder = p.nodeOrder[1:]
		delete(p.nodeRefs, drop)
	}
	return id
}

func (p *managedPage) nodeReference(id string) (nodeReference, error) {
	p.nodeMu.Lock()
	defer p.nodeMu.Unlock()
	ref, ok := p.nodeRefs[strings.TrimSpace(id)]
	if !ok {
		return nodeReference{}, fmt.Errorf("browser: node_id is invalid or stale; take a fresh visible DOM snapshot")
	}
	return ref, nil
}
