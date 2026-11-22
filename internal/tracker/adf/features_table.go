package adf

func tableFeatures(n *Node, add func(string, bool)) {
	for k, v := range n.Attrs {
		switch k {
		case "localId":
		case "isNumberColumnEnabled":
			if v == true {
				add("numbered table rows", true)
			}
		case "layout", "width", "displayMode":
			add("table width or layout", false)
		}
	}
	for r, row := range n.Content {
		allHeader, anyHeader, empty := true, false, true
		for _, c := range row.Content {
			if c.Type == "tableHeader" {
				anyHeader = true
			} else {
				allHeader = false
			}
			if len(c.Content) != 1 || c.Content[0].Type != "paragraph" ||
				len(c.Content[0].Content) != 0 {
				empty = false
			}
		}
		if anyHeader && (r > 0 || !allHeader) {
			add("header cells outside the first row", true)
		}
		if r == 0 && allHeader && empty && len(n.Content) > 1 {
			add("an empty header row", false)
		}
		if len(row.Content) == 0 {
			add("empty table rows", false)
		}
	}
	if len(n.Content) > 0 {
		cols := -1
		for _, row := range n.Content {
			w := 0
			for _, c := range row.Content {
				w += span(c, "colspan")
			}
			if cols >= 0 && w != cols {
				add("ragged table rows", false)
			}
			cols = w
		}
	}
}

func cellFeatures(n *Node, add func(string, bool)) {
	for k := range n.Attrs {
		switch k {
		case "colwidth":
			add("table column widths", true)
		case "background":
			add("cell colors", true)
		case "valign":
			add("cell alignment", true)
		case "colspan", "rowspan":
			if f, ok := n.attrNum(k); !ok || f != 1 {
				add("merged cells", true)
			}
		}
	}
	paras := 0
	for _, c := range n.Content {
		if c.Type != "paragraph" {
			add("blocks inside table cells", true)
			return
		}
		paras++
	}
	if paras > 1 {
		add("paragraph breaks in table cells", false)
	}
}
