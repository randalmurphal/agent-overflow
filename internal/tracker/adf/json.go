package adf

import (
	"bytes"
	"encoding/json"
)

// contentRequired lists node types whose content key the schema
// requires, so an empty one must still be written as "content":[].
var contentRequired = map[string]bool{
	"doc": true, "blockTaskItem": true, "blockquote": true,
	"bodiedExtension": true, "bodiedSyncBlock": true, "bulletList": true,
	"decisionList": true, "expand": true, "layoutColumn": true,
	"layoutSection": true, "listItem": true, "mediaGroup": true,
	"mediaSingle": true, "nestedExpand": true, "orderedList": true,
	"panel": true, "table": true, "tableCell": true, "tableHeader": true,
	"tableRow": true, "taskList": true,
}

// MarshalJSON writes n in ADF's JSON shape. Content is omitted when empty
// except on node types that require it, which Jira and Confluence reject
// without it.
// Text keeps <, > and & as written, so List<String> reads as itself in
// dry runs and --json output.
func (n *Node) MarshalJSON() ([]byte, error) {
	type plain Node
	if !contentRequired[n.Type] || len(n.Content) > 0 {
		return marshal((*plain)(n))
	}
	return marshal(struct {
		Type    string         `json:"type"`
		Version int            `json:"version,omitempty"`
		Attrs   map[string]any `json:"attrs,omitempty"`
		Content []*Node        `json:"content"`
		Marks   []Mark         `json:"marks,omitempty"`
		Text    string         `json:"text,omitempty"`
	}{n.Type, n.Version, n.Attrs, []*Node{}, n.Marks, n.Text})
}

// marshal is json.Marshal without HTML escaping.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
