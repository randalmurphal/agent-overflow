package provider

import (
	"regexp"
	"strings"
)

var proposedPlanBlockRE = regexp.MustCompile(`(?is)<proposed_plan>\s*([\s\S]*?)\s*</proposed_plan>`)

// ExtractProposedPlanMarkdown returns the markdown wrapped by a proposed plan
// block, or an empty string when no block is present.
func ExtractProposedPlanMarkdown(text string) string {
	match := proposedPlanBlockRE.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// StripProposedPlanBlocks removes any proposed plan blocks from the source
// text and trims the remaining content.
func StripProposedPlanBlocks(text string) string {
	stripped := proposedPlanBlockRE.ReplaceAllString(text, "")
	return strings.TrimSpace(stripped)
}
