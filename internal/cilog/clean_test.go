package cilog

import (
	"strconv"
	"strings"
	"testing"
)

// recordSink renders what a cleaner emits: each display line, and each
// marker as a line of its own.
type recordSink struct {
	out  []string
	line strings.Builder
}

func (r *recordSink) text(p []byte) error { r.line.Write(p); return nil }

func (r *recordSink) endLine() error {
	r.out = append(r.out, r.line.String())
	r.line.Reset()
	return nil
}

func (r *recordSink) marker(start bool, unix int64, name string) error {
	kind := "end"
	if start {
		kind = "start"
	}
	r.out = append(r.out, "section_"+kind+":"+strconv.FormatInt(unix, 10)+":"+name)
	return nil
}

// cleanInChunks runs raw through a trace cleaner in writes of chunk bytes.
func cleanInChunks(t *testing.T, raw string, chunk int) string {
	t.Helper()
	sink := &recordSink{}
	cleaner := newTraceCleaner(sink)
	for len(raw) > 0 {
		n := min(chunk, len(raw))
		if _, err := cleaner.Write([]byte(raw[:n])); err != nil {
			t.Fatal(err)
		}
		raw = raw[n:]
	}
	if err := cleaner.close(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(sink.out, "\n")
}

func TestTraceCleaner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "timestamped stream prefix keeps timestamp, drops flags",
			raw:  "2026-06-26T22:39:47.158568Z 00O echo hello",
			want: "2026-06-26T22:39:47.158568Z echo hello",
		},
		{
			name: "marker-only timestamped line keeps the bare marker",
			raw: "2026-06-26T22:39:47.158568Z 00O+\x1b[0Ksection_start:1782513587:upload_artifacts_on_success\n" +
				"2026-06-26T22:39:47.158568Z 00O \x1b[0;33mUploading artifacts\x1b[0;m",
			want: "section_start:1782513587:upload_artifacts_on_success\n" +
				"2026-06-26T22:39:47.158568Z \x1b[0;33mUploading artifacts\x1b[0;m",
		},
		{
			name: "inline section marker puts its CR-erased header on the next line",
			raw:  "section_start:1714557600:step_script\r\x1b[0K\x1b[36;1mRunning steps\x1b[0;m",
			want: "section_start:1714557600:step_script\n\x1b[36;1mRunning steps\x1b[0;m",
		},
		{
			name: "section marker options are read and dropped",
			raw:  "section_start:1714557600:cleanup[collapsed=true]\r\x1b[0Kdone",
			want: "section_start:1714557600:cleanup\ndone",
		},
		{
			name: "an end and a start on one line keep their order",
			raw:  "\x1b[0Ksection_end:1714557605:prepare\r\x1b[0K\x1b[0Ksection_start:1714557605:step_script\r\x1b[0K\x1b[0K\x1b[36;1mExecuting\x1b[0;m",
			want: "section_end:1714557605:prepare\nsection_start:1714557605:step_script\n\x1b[36;1mExecuting\x1b[0;m",
		},
		{
			name: "output without a final newline stays before the end marker",
			raw:  "2026-06-26T22:39:47.158568Z 00O last words\x1b[0Ksection_end:1782513590:step_script\r\x1b[0K",
			want: "2026-06-26T22:39:47.158568Z last words\nsection_end:1782513590:step_script",
		},
		{
			name: "carriage-return progress overwrite keeps the final frame",
			raw:  "Downloading  10%\rDownloading  60%\rDownloading 100%",
			want: "Downloading 100%",
		},
		{
			name: "text before a marker overwritten by a carriage return",
			raw:  "foo\rbar section_start:1:x\rbaz",
			want: "bar \nsection_start:1:x\nbaz",
		},
		{
			name: "text after a marker is overwritten by a later carriage return",
			raw:  "section_start:1:x tail\rheader",
			want: "section_start:1:x\nheader",
		},
		{
			name: "blank lines and plain lines pass through",
			raw:  "line one\n\nline two\n",
			want: "line one\n\nline two",
		},
		{
			name: "a line ending in a carriage return is blank",
			raw:  "foo\r\nnext",
			want: "\nnext",
		},
		{
			name: "erase-in-line escapes are stripped",
			raw:  "\x1b[0Kfoo \x1b[Kbar",
			want: "foo bar",
		},
		{
			name: "section_start without a time is text",
			raw:  "section_start without a time stays",
			want: "section_start without a time stays",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, len(tt.raw) + 1} {
				if got := cleanInChunks(t, tt.raw, chunk); got != tt.want {
					t.Fatalf("in chunks of %d: %q, want %q", chunk, got, tt.want)
				}
			}
		})
	}
}

// A run of text with no carriage return or newline is held only up to
// traceSegmentMax; past it the run is a display line of its own.
func TestTraceCleanerBoundsARunWithoutLineEnds(t *testing.T) {
	t.Parallel()
	run := strings.Repeat("x", 2*traceSegmentMax+10)
	sink := &recordSink{}
	cleaner := newTraceCleaner(sink)
	for i := 0; i < len(run); i += 4096 {
		if _, err := cleaner.Write([]byte(run[i:min(i+4096, len(run))])); err != nil {
			t.Fatal(err)
		}
		if len(cleaner.seg) > traceSegmentMax+4096 {
			t.Fatalf("the cleaner holds %d bytes of one run", len(cleaner.seg))
		}
	}
	if err := cleaner.close(); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range sink.out {
		if len(line) > traceSegmentMax+4096 {
			t.Fatalf("a display line of %d bytes", len(line))
		}
		total += len(line)
	}
	if len(sink.out) != 2 || total != len(run) {
		t.Fatalf("a long run became %d lines of %d bytes", len(sink.out), total)
	}
}

func TestLineSplitterDropsALeadingBOMAcrossWrites(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		writes []string
		want   []string
	}{
		"BOM split across writes":  {[]string{"\xef", "\xbb\xbfa\nb"}, []string{"a", "b"}},
		"no BOM":                   {[]string{"a", "b\n"}, []string{"ab"}},
		"a log shorter than a BOM": {[]string{"\xef\xbb"}, []string{"\xef\xbb"}},
		"a BOM later is text":      {[]string{"a\n\xef\xbb\xbfb\n"}, []string{"a", "\xef\xbb\xbfb"}},
		"empty":                    {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &recordSink{}
			split := &lineSplitter{sink: sink}
			for _, w := range tc.writes {
				if _, err := split.Write([]byte(w)); err != nil {
					t.Fatal(err)
				}
			}
			if err := split.close(); err != nil {
				t.Fatal(err)
			}
			if strings.Join(sink.out, "|") != strings.Join(tc.want, "|") || len(sink.out) != len(tc.want) {
				t.Fatalf("lines %q, want %q", sink.out, tc.want)
			}
		})
	}
}
