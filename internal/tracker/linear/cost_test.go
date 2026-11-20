package linear

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The cost benchmark measures call's decode of an issue page as every
// issue read returns it (the poll, board rows, cards, search): a 100-issue
// page with the description each issue carried before lists left it out
// and without it, and a 250-issue page, the size lists now ask for.

func costIssuePage(n int, description bool) []byte {
	var b bytes.Buffer
	b.WriteString(`{"data":{"page":{"nodes":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"00000000-0000-4000-8000-%012x","identifier":"WEB-%d","number":%d,"title":"Issue %d with a title of ordinary length",`, 1000+i, i+1, i+1, i+1)
		if description {
			fmt.Fprintf(&b, `"description":%q,`, strings.Repeat("A paragraph of plain words that describes the work to be done. ", 30))
		}
		fmt.Fprintf(&b, `"url":"https://linear.app/example/issue/WEB-%d/issue","branchName":"user/web-%d-issue",`, i+1, i+1)
		b.WriteString(`"priority":2,"estimate":3,"sortOrder":1000.5,"prioritySortOrder":10,"subIssueSortOrder":null,`)
		b.WriteString(`"team":{"id":"00000000-0000-4000-8000-00000000000a","issueEstimationType":"fibonacci"},"state":{"id":"00000000-0000-4000-8000-000000000015"},`)
		b.WriteString(`"assignee":{"id":"00000000-0000-4000-8000-000000000001"},"creator":{"id":"00000000-0000-4000-8000-000000000001"},"cycle":null,"project":null,"parent":null,`)
		b.WriteString(`"labelIds":["00000000-0000-4000-8000-000000000028"],"labels":{"nodes":[{"id":"00000000-0000-4000-8000-000000000028","name":"Bug"}]},`)
		b.WriteString(`"dueDate":null,"createdAt":"2026-10-01T09:00:00.000Z","updatedAt":"2026-11-18T09:00:00.000Z","startedAt":null,"completedAt":null,"canceledAt":null,"archivedAt":null,"trashed":null}`)
	}
	b.WriteString(`],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`)
	return b.Bytes()
}

// BenchmarkDecodeIssuePage is call's decode of one page: decodeAnswer's
// single pass over the answer into the page, then each issue converted.
func BenchmarkDecodeIssuePage(b *testing.B) {
	for _, c := range []struct {
		n    int
		desc bool
	}{{100, true}, {100, false}, {250, false}} {
		body := costIssuePage(c.n, c.desc)
		b.Run(fmt.Sprintf("issues=%d/description=%v-%dKB", c.n, c.desc, len(body)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				var data rootPage[wireIssue]
				present, wire, err := decodeAnswer(body, &data)
				if err != nil || !present || wire != nil {
					b.Fatal(present, wire, err)
				}
				p, err := page(opIssues, *data.Page, Usage{}, wireIssue.typed)
				if err != nil || len(p.Items) != c.n {
					b.Fatal(err)
				}
			}
		})
	}
}
