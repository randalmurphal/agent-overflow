package adf

import "strings"

// renderTable renders a GFM table. GFM needs a header row: when the
// first ADF row is not all header cells, the table gets an empty header
// row, which FromMarkdown reads back as "no header".
func renderTable(n *Node) []string {
	var rows []*Node
	for _, r := range n.Content {
		if r.Type == "tableRow" {
			rows = append(rows, r)
		}
	}
	grid, cols := tableGrid(rows)
	if cols == 0 {
		return nil
	}
	header := len(rows) > 0 && len(rows[0].Content) > 0
	if header {
		for _, c := range rows[0].Content {
			header = header && c.Type == "tableHeader"
		}
	}
	line := func(cells []string) string {
		return "| " + strings.Join(cells, " | ") + " |"
	}
	delim := make([]string, cols)
	for i := range delim {
		delim[i] = "---"
	}
	var out []string
	body := grid
	if header {
		out = append(out, line(grid[0]))
		body = grid[1:]
	} else {
		out = append(out, line(make([]string, cols)))
	}
	out = append(out, line(delim))
	for _, r := range body {
		out = append(out, line(r))
	}
	return out
}

// tableGrid places cells on a grid, leaving merged positions empty.
func tableGrid(rows []*Node) ([][]string, int) {
	taken := make([][]bool, len(rows)) // positions covered by a span
	type placed struct {
		r, c int
		s    string
	}
	var cells []placed
	cols := 0
	for r, row := range rows {
		c := 0
		for _, cell := range row.Content {
			for c < len(taken[r]) && taken[r][c] {
				c++
			}
			cs := span(cell, "colspan")
			rsp := min(span(cell, "rowspan"), len(rows)-r)
			for dr := 0; dr < rsp; dr++ {
				t := taken[r+dr]
				for len(t) < c+cs {
					t = append(t, false)
				}
				for dc := 0; dc < cs; dc++ {
					t[c+dc] = true
				}
				taken[r+dr] = t
			}
			cells = append(cells, placed{r, c, renderCell(cell)})
			c += cs
		}
		cols = max(cols, c)
	}
	for _, t := range taken {
		for c := len(t) - 1; c >= cols; c-- {
			if t[c] {
				cols = c + 1
				break
			}
		}
	}
	grid := make([][]string, len(rows))
	for i := range grid {
		grid[i] = make([]string, cols)
	}
	for _, p := range cells {
		grid[p.r][p.c] = p.s
	}
	return grid, cols
}

func span(cell *Node, k string) int {
	if f, ok := cell.attrNum(k); ok && f >= 1 && f < 1000 {
		return int(f)
	}
	return 1
}

// renderCell renders a cell on one line. Paragraphs and hard breaks join
// with <br>; lists, code and other blocks become readable text.
func renderCell(cell *Node) string {
	return strings.Join(cellLines(cell.Content, ""), "<br>")
}

func cellLines(ns []*Node, indent string) []string {
	var out []string
	for _, n := range ns {
		switch n.Type {
		case "paragraph":
			if s := renderInline(n.Content, ctxCell); s != "" {
				out = append(out, indent+s)
			}
		case "heading":
			out = append(out, indent+renderInline(embolden(n.Content),
				ctxCell))
		case "bulletList", "orderedList":
			out = append(out, listCell(n, indent)...)
		case "taskList":
			out = append(out, taskCell(n, indent)...)
		case "decisionList":
			for _, it := range n.Content {
				out = append(out, indent+"Decision: "+
					renderInline(it.Content, ctxCell))
			}
		case "codeBlock":
			for _, l := range strings.Split(codeText(n), "\n") {
				if l != "" {
					out = append(out, indent+codeSpan(l, true))
				}
			}
		case "mediaSingle":
			out = append(out, indent+cellPipes(renderMediaSingle(n),
				ctxCell))
			for _, c := range n.Content {
				if c.Type == "caption" {
					if s := renderInline(c.Content, ctxCell); s != "" {
						out = append(out, indent+s)
					}
				}
			}
		case "mediaGroup":
			for _, m := range n.Content {
				out = append(out, indent+cellPipes(mediaLink(m), ctxCell))
			}
		case "rule":
		case "expand", "nestedExpand":
			if t := n.attrStr("title"); t != "" {
				out = append(out, indent+renderInline(
					[]*Node{textNode(t, Mark{Type: "strong"})}, ctxCell))
			}
			out = append(out, cellLines(n.Content, indent)...)
		case "extension", "bodiedExtension":
			out = append(out, indent+macroComment(n))
			out = append(out, cellLines(n.Content, indent)...)
		default:
			if isInline(n.Type) {
				out = append(out, indent+renderInline([]*Node{n}, ctxCell))
			} else {
				out = append(out, cellLines(n.Content, indent)...)
			}
		}
	}
	return out
}

func listCell(n *Node, indent string) []string {
	num := 1
	if f, ok := n.attrNum("order"); ok {
		num = int(f)
	}
	var out []string
	for _, it := range n.Content {
		marker := "- "
		if n.Type == "orderedList" {
			marker = listNumber(num) + ". "
			num++
		}
		out = append(out, itemCell(indent, marker, it.Content)...)
	}
	return out
}

func taskCell(n *Node, indent string) []string {
	var out []string
	for _, it := range n.Content {
		if it.Type == "taskList" {
			out = append(out, taskCell(it, indent+"  ")...)
			continue
		}
		out = append(out, indent+checkbox(it)+" "+
			renderInline(it.Content, ctxCell))
	}
	return out
}

func itemCell(indent, marker string, content []*Node) []string {
	var out []string
	for i, c := range content {
		ls := cellLines([]*Node{c}, indent+"  ")
		if i == 0 && len(ls) > 0 && c.Type == "paragraph" {
			ls[0] = indent + marker + strings.TrimPrefix(ls[0], indent+"  ")
		} else if i == 0 {
			out = append(out, indent+strings.TrimSpace(marker))
		}
		out = append(out, ls...)
	}
	if len(content) == 0 {
		out = append(out, indent+strings.TrimSpace(marker))
	}
	return out
}
