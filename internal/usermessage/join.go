package usermessage

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// JoinSeparator is the visible rule between the parts of a JOINED queued
// message. It goes in BOTH the stored row summary and the text written to the
// provider, so the bubble the user reads back, the text the model receives,
// and the message a revert cuts at are one thing.
//
// Claude Code's own boundary-drain merge newline-joins the parts
// (`joinPromptValues`, src/cli/print.ts). AO uses a markdown rule instead
// because its copy is also the row a person reads, edits and reverts to, and
// two prompts run together across a bare newline read as one paragraph.
//
// It lives here rather than beside either joiner because two of them exist for
// one wire fact: the flush dispatcher joins a drain it can see coming
// (`internal/app/app_flush_dispatch_join.go`), and triage folds one the CLI
// made across separate drains (`internal/triage/claude_merge_fold.go`). The
// two must produce the same row.
const JoinSeparator = "\n\n---\n\n"

// TextPart is one member's contribution to a joined message: the text to
// concatenate and how many images its inline `[Image #N]` markers index into.
type TextPart struct {
	Text string
	// ImageCount is the number of images this part's markers address — the
	// length of the provider attachment list the part was written against,
	// NOT the count of markers actually present (a part can carry fewer
	// markers than images, and the renumber walk needs the list length).
	ImageCount int
}

// JoinRenumberedText concatenates the parts and RENUMBERS their inline image
// markers into the joined attachment order (every part's images in order).
//
// The composer numbers `[Image #N]` per message, so a plain concatenation
// leaves two `#1` markers and an image with no marker at all: a reader's split
// claims the first unused occurrence per index, binds attachment 1 to the first
// part's marker, finds no `#2`, and appends the second part's image at the end
// while its literal `#1` stays in the text. Re-emitting each marker from the
// part's attachment offset keeps every image where the user dropped it.
func JoinRenumberedText(parts []TextPart) string {
	var text strings.Builder
	base := 0
	for i, part := range parts {
		if i > 0 {
			text.WriteString(JoinSeparator)
		}
		for _, split := range provider.SplitContentByImageMarkers(part.Text, part.ImageCount) {
			if split.ImageIndex >= 0 {
				text.WriteString(provider.ImagePlaceholderLabel(base + split.ImageIndex + 1))
				continue
			}
			text.WriteString(split.Text)
		}
		base += part.ImageCount
	}
	return text.String()
}

// ImageAttachmentCount reports how many of a row's persisted attachments are
// images — the only ones an `[Image #N]` marker can address, and therefore the
// ImageCount a TextPart rebuilt from a stored row must carry.
//
// An empty Kind means image (see AttachmentMeta.Kind), so the test is for a
// kind that is explicitly something else.
func ImageAttachmentCount(attachments []AttachmentMeta) int {
	n := 0
	for _, attachment := range attachments {
		if attachment.Kind == "" || attachment.Kind == store.AttachmentKindImage {
			n++
		}
	}
	return n
}

// Projection returns the Meta that Marshal would encode for this Input. Split
// out so a caller that must UNION several members' metadata before encoding
// (JoinMetas) works on the same shape Marshal writes, instead of a second
// projection that could drift from it.
func (in Input) Projection() Meta {
	metaAttachments := make([]AttachmentMeta, 0, len(in.Attachments))
	for _, attachment := range in.Attachments {
		metaAttachments = append(metaAttachments, AttachmentMeta{
			ID:       attachment.ID,
			ThreadID: attachment.ThreadID,
			Filename: attachment.Filename,
			MimeType: attachment.MimeType,
			Size:     attachment.Size,
			Kind:     attachment.Kind,
		})
	}
	return Meta{
		Attachments:                  metaAttachments,
		SourceProposedPlan:           in.SourcePlan,
		RevisionSourceProposedPlan:   in.RevisionSourcePlan,
		RevisionSourceCommentIDs:     in.RevisionCommentIDs,
		RevisionSourceDiffReview:     in.RevisionSourceDiff,
		RevisionSourceDiffCommentIDs: in.RevisionDiffCommentIDs,
		Command:                      in.Command,
		ExpandComposerCommands:       in.ExpandComposerCommands,
		SendID:                       in.SendID,
		JoinedSendIDs:                in.JoinedSendIDs,
	}
}

// JoinMetas unions the members' row metadata in queue order.
//
// Attachments concatenate in that order, matching the markers
// JoinRenumberedText renumbers. The singular references (source plan,
// revision plan, diff review) take the first member that carries one, and
// comment ids are collected only from the members pointing at THAT reference:
// the badges a row renders are scoped to its own reference, so folding in
// another plan's comment ids would label the row with comments it does not
// show. Each member's plan/comment bookkeeping is still applied separately by
// its own acceptance path.
//
// SendID is the FIRST member's, so the indexed fast path keeps answering the
// common retry; JoinedSendIDs collects every member's ids (a member that is
// itself already a joined row contributes all the ids it answered for) so a
// retry of any LATER member resolves to this row instead of duplicating it.
func JoinMetas(members []Meta) Meta {
	var joined Meta
	for _, member := range members {
		joined.Attachments = append(joined.Attachments, member.Attachments...)
		if joined.SourceProposedPlan == nil {
			joined.SourceProposedPlan = member.SourceProposedPlan
		}
		if member.RevisionSourceProposedPlan != nil {
			if joined.RevisionSourceProposedPlan == nil {
				joined.RevisionSourceProposedPlan = member.RevisionSourceProposedPlan
			}
			if samePlanRef(joined.RevisionSourceProposedPlan, member.RevisionSourceProposedPlan) {
				joined.RevisionSourceCommentIDs = append(joined.RevisionSourceCommentIDs, member.RevisionSourceCommentIDs...)
			}
		}
		if member.RevisionSourceDiffReview != nil {
			if joined.RevisionSourceDiffReview == nil {
				joined.RevisionSourceDiffReview = member.RevisionSourceDiffReview
			}
			if sameDiffRef(joined.RevisionSourceDiffReview, member.RevisionSourceDiffReview) {
				joined.RevisionSourceDiffCommentIDs = append(joined.RevisionSourceDiffCommentIDs, member.RevisionSourceDiffCommentIDs...)
			}
		}
		if joined.Command == "" {
			joined.Command = member.Command
		}
		// A crash-rebuilt row must keep the composer's slash semantics for
		// any member that came through the composer.
		joined.ExpandComposerCommands = joined.ExpandComposerCommands || member.ExpandComposerCommands
		ids := member.JoinedSendIDs
		if len(ids) == 0 {
			ids = []string{member.SendID}
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			if joined.SendID == "" {
				joined.SendID = id
			}
			if !slices.Contains(joined.JoinedSendIDs, id) {
				joined.JoinedSendIDs = append(joined.JoinedSendIDs, id)
			}
		}
	}
	return joined
}

func samePlanRef(a, b *store.ProposedPlanSourceRef) bool {
	return a != nil && b != nil && a.ThreadID == b.ThreadID && a.ItemID == b.ItemID
}

func sameDiffRef(a, b *store.DiffReviewSourceRef) bool {
	return a != nil && b != nil && a.Scope == b.Scope && a.SourceKey == b.SourceKey
}

// MarshalMeta encodes an already-projected Meta with the same empty-is-nothing
// rule Marshal applies: a Meta with no field set returns ("", nil) so the
// caller can persist SQL NULL.
func MarshalMeta(meta Meta) (string, error) {
	if meta.isZero() {
		return "", nil
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (m Meta) isZero() bool {
	return len(m.Attachments) == 0 &&
		m.SourceProposedPlan == nil &&
		m.RevisionSourceProposedPlan == nil &&
		len(m.RevisionSourceCommentIDs) == 0 &&
		m.RevisionSourceDiffReview == nil &&
		len(m.RevisionSourceDiffCommentIDs) == 0 &&
		m.Command == "" &&
		m.SendID == "" &&
		len(m.JoinedSendIDs) == 0 &&
		!m.ExpandComposerCommands
}

// metaTypedKeys is every JSON key the typed Meta struct owns. The item meta
// blob is a superset: `provider_item_id`, `provider_parent_uuid` and the
// promotion markers ride as top-level keys beside these (see the package doc),
// and a joined rewrite must replace only what Meta describes.
//
// Pinned against the struct's own tags by TestMetaTypedKeysCoverEveryField, so
// a new Meta field cannot silently survive a rewrite that should have replaced
// it.
var metaTypedKeys = []string{
	"attachments",
	"sourceProposedPlan",
	"revisionSourceProposedPlan",
	"revisionSourceCommentIds",
	"revisionSourceDiffReview",
	"revisionSourceDiffCommentIds",
	"command",
	"expandComposerCommands",
	"sendId",
	"joinedSendIds",
}

// MergeJoinedMeta rewrites the typed usermessage fields of `existing` to those
// of `joined`, preserving every OTHER top-level key on the blob.
//
// Used when a row that already carries wire correlation is rebuilt as a join:
// the survivor of a CLI queue-boundary merge keeps its `provider_item_id`,
// `provider_parent_uuid` and any promotion markers — those describe where the
// provider consumed the message, which the fold does not change — while its
// text, attachments, source references and send ids become the union of every
// folded member's.
//
// Re-marshalling a decoded Meta cannot do this: the typed struct has no home
// for the correlation keys, so the encode would drop them and leave the row
// with no slice anchor.
func MergeJoinedMeta(existing string, joined Meta) (string, error) {
	fields := map[string]any{}
	if trimmed := strings.TrimSpace(existing); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
			return "", fmt.Errorf("decode existing meta: %w", err)
		}
		if fields == nil {
			fields = map[string]any{}
		}
	}
	for _, key := range metaTypedKeys {
		delete(fields, key)
	}
	encoded, err := MarshalMeta(joined)
	if err != nil {
		return "", err
	}
	if encoded != "" {
		var typed map[string]any
		if err := json.Unmarshal([]byte(encoded), &typed); err != nil {
			return "", fmt.Errorf("decode joined meta: %w", err)
		}
		for key, value := range typed {
			fields[key] = value
		}
	}
	if len(fields) == 0 {
		return "", nil
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("encode joined meta: %w", err)
	}
	return string(merged), nil
}
