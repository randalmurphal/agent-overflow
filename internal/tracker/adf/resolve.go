package adf

// resolvePending resolves mentions and uploads left pending in blocks
// taken from md, trying names already mentioned in orig first.
func (c *converter) resolvePending(newAt []bool, fresh []*Node,
	orig *Node) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	known := knownMentions(orig)
	c.deferR = false
	var err error
	for _, kind := range []string{"mention", "media"} {
		for j, b := range fresh {
			if !newAt[j] || err != nil {
				continue
			}
			walk(b, func(n, parent *Node) {
				if err != nil || n.Type != kind || n.attrStr("id") != pendingID {
					return
				}
				switch n.Type {
				case "mention":
					who := n.attrStr(attrWho)
					if k, ok := known[who]; ok {
						n.Attrs = map[string]any{"id": k[0], "text": k[1]}
						return
					}
					var m *Node
					if m, err = c.mention(who, c.lineFor(n)); err == nil {
						n.Attrs = m.Attrs
					}
				case "media":
					path := n.attrStr(attrPath)
					var id string
					if id, err = c.upload(path, c.lineFor(n)); err == nil {
						delete(n.Attrs, attrPath)
						n.Attrs["id"] = id
					}
				}
			})
		}
	}
	return err
}

// knownMentions maps each display name mentioned in orig to the account
// and text it was mentioned with. A name mentioned for two accounts is
// left out, so the Resolver decides (or refuses, naming the candidates)
// instead of the last one seen winning.
func knownMentions(orig *Node) map[string][2]string {
	known := map[string][2]string{}
	if orig == nil {
		return known
	}
	shared := map[string]bool{}
	walk(orig, func(n, _ *Node) {
		if n.Type != "mention" {
			return
		}
		name := mentionName(n)
		if name == "" || shared[name] {
			return
		}
		id := n.attrStr("id")
		if k, ok := known[name]; ok && k[0] != id {
			delete(known, name)
			shared[name] = true
			return
		}
		known[name] = [2]string{id, n.attrStr("text")}
	})
	return known
}
