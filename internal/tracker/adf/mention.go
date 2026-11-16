package adf

import (
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// mentionNode is the goldmark node for @[who].
type mentionNode struct {
	gast.BaseInline
	who string
}

var kindMention = gast.NewNodeKind("Mention")

func (n *mentionNode) Kind() gast.NodeKind { return kindMention }

func (n *mentionNode) Dump(src []byte, level int) {
	gast.DumpHelper(n, src, level, map[string]string{"Who": n.who}, nil)
}

// mentionParser recognizes @[who]. Inside the brackets, backslash
// escapes work as in Markdown.
type mentionParser struct{}

func (mentionParser) Trigger() []byte { return []byte{'@'} }

func (mentionParser) Parse(_ gast.Node, block text.Reader,
	_ parser.Context) gast.Node {
	line, seg := block.PeekLine()
	if len(line) < 3 || line[1] != '[' {
		return nil
	}
	var who []byte
	for i := 2; i < len(line); i++ {
		switch b := line[i]; {
		case b == '\\' && i+1 < len(line) && isASCIIPunct(line[i+1]):
			who = append(who, line[i+1])
			i++
		case b == ']':
			n := &mentionNode{who: string(who)}
			n.SetPos(seg.Start)
			block.Advance(i + 1)
			return n
		case b == '[' || b == '\n':
			return nil
		default:
			who = append(who, b)
		}
	}
	return nil
}

func isASCIIPunct(b byte) bool {
	return b >= '!' && b <= '/' || b >= ':' && b <= '@' ||
		b >= '[' && b <= '`' || b >= '{' && b <= '~'
}
