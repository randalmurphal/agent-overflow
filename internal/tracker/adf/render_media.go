package adf

import (
	"strings"
)

func renderMediaSingle(n *Node) string {
	var media *Node
	for _, c := range n.Content {
		if c.Type == "media" {
			media = c
			break
		}
	}
	if media == nil {
		return ""
	}
	img := "![" + escapeAlt(media.attrStr("alt")) + "](" +
		linkDest(imageSrc(media), false) + ")"
	for _, m := range n.Marks {
		if m.Type == "link" {
			href, _ := m.Attrs["href"].(string)
			img = "[" + img + "](" + linkDest(href, false) + ")"
		}
	}
	return img
}

// renderCaption renders an image caption as the paragraph after it.
func renderCaption(ms *Node) string {
	for _, c := range ms.Content {
		if c.Type == "caption" {
			return renderInline(c.Content, ctxPara)
		}
	}
	return ""
}

// externalPrefix marks an external image whose URL has no scheme
// (/local/path), or starts media: or external: itself: written bare it
// would read back as a local file to upload, or as an attachment.
const externalPrefix = "external:"

// imageSrc is an image's destination: media:ID, a URL, or external:URL
// for an external URL that would read as something else.
func imageSrc(m *Node) string {
	src := mediaSrc(m)
	if m.attrStr("type") == "external" && (!hasScheme(src) ||
		strings.HasPrefix(src, "media:") || strings.HasPrefix(src, externalPrefix)) {
		return externalPrefix + src
	}
	return src
}

// schemeRe is a URL scheme of two or more characters, so a Windows drive
// (C:\x.png) is still a path.
var schemeRe = lazyRegexp{pattern: `^[a-zA-Z][a-zA-Z0-9+.-]+:`}

func hasScheme(s string) bool { return schemeRe.get().MatchString(s) }

func mediaSrc(m *Node) string {
	if m.attrStr("type") == "external" {
		return m.attrStr("url")
	}
	return "media:" + m.attrStr("id")
}

func renderMediaGroup(n *Node) blk {
	var ls []string
	for i, m := range n.Content {
		l := mediaLink(m)
		if i < len(n.Content)-1 {
			l += `\`
		}
		ls = append(ls, l)
	}
	return lines(ls...)
}

// mediaLink renders a non-image attachment as a media: link.
func mediaLink(m *Node) string {
	label := m.attrStr("alt")
	if label == "" {
		label = "attachment"
	}
	return "[" + escapeAlt(label) + "](" + linkDest(mediaSrc(m), false) + ")"
}
