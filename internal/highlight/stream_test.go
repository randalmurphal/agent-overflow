package highlight

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// streamSamples holds, per grammar, source with the multi-line constructs
// whose classes depend on text that arrives later: block comments, multi-line
// strings and templates, nested blocks, injections, and definitions whose
// names are classified only once their parameter list or body arrives.
var streamSamples = map[Lang]string{
	LangTypeScript: "import { a, type B } from './mod';\n/**\n * Doc comment.\n */\nexport class Store<T extends object> implements B {\n  private readonly items = new Map<string, T>();\n  constructor(private name: string) {}\n  get(id: string): T | undefined {\n    const tpl = `item ${id} of ${this.name}\n      spans lines`;\n    return this.items.get(id) ?? undefined;\n  }\n}\nenum Kind { A = 1, B }\nconst re = /ab+c/gi; // trailing\n",
	LangTSX:        "import React from 'react';\ntype Props = { title: string; items: string[] };\nexport function List({ title, items }: Props) {\n  return (\n    <section className=\"list\">\n      <h1>{title}</h1>\n      {items.map((item) => (\n        <Item key={item} label={`#${item}`} />\n      ))}\n    </section>\n  );\n}\n",
	LangJavaScript: "'use strict';\n/* block\n   comment */\nfunction add(a, b = 2) {\n  return a + b;\n}\nconst obj = {\n  key: 'value',\n  nested: { deep: [1, 2, 3] },\n  method() { return this.key; },\n};\nclass A extends Base {\n  #priv = 1;\n  static create() { return new A(); }\n}\nconst el = <div id=\"x\">{obj.key}</div>;\nexport default add;\n",
	LangJSON:       "{\n  \"name\": \"sample\",\n  \"version\": 3,\n  \"nested\": {\n    \"list\": [1, 2.5, -3e4, true, false, null],\n    \"escaped\": \"line\\nbreak \\\"quoted\\\"\"\n  }\n}\n",
	LangGo:         "package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n// Doc comment for T.\ntype T struct {\n\tName string `json:\"name\"`\n\tn    int\n}\n\n/* block\n   comment */\nfunc (t *T) Greet(prefix string) (string, error) {\n\traw := `raw\nstring`\n\tfor i := 0; i < t.n; i++ {\n\t\tprefix += strings.Repeat(\"x\", i)\n\t}\n\treturn fmt.Sprintf(\"%s %s %c\", prefix, raw, 'r'), nil\n}\n",
	LangPython:     "import os\nfrom typing import Optional\n\n\n@decorator(arg=1)\nclass Thing(Base):\n    \"\"\"Docstring that\n    spans lines.\"\"\"\n\n    def method(self, x: int, *args, **kw) -> Optional[str]:\n        if x > 0 and not args:\n            return f\"value {x!r} {kw}\"\n        lam = lambda y: y * 2\n        return None  # comment\n\n\nprint(Thing().method(3))\n",
	LangBash:       "#!/usr/bin/env bash\nset -euo pipefail\n# comment\nname=\"world\"\nread -r -d '' heredoc <<'EOF' || true\nline one\nline two\nEOF\nfor f in *.txt; do\n  if [[ -f \"$f\" ]]; then\n    echo \"${f%.txt}: $(wc -l < \"$f\")\"\n  fi\ndone\ngreet() { local who=$1; printf 'hi %s\\n' \"$who\"; }\ngreet \"$name\"\n",
	LangCSS:        "/* theme */\n:root {\n  --accent: #3b82f6;\n}\n@media (max-width: 600px) {\n  .card > .title::before,\n  a[href^=\"https\"]:hover {\n    color: var(--accent);\n    margin: 0 auto !important;\n    transform: rotate(45deg) scale(1.5);\n  }\n}\n@keyframes spin { from { opacity: 0; } to { opacity: 1; } }\n",
	LangHTML:       "<!doctype html>\n<html lang=\"en\">\n<head>\n  <style>\n    body { color: red; }\n  </style>\n  <!-- comment\n       spanning lines -->\n</head>\n<body>\n  <p class=\"x\">Text &amp; entity</p>\n  <script>\n    const n = 1;\n    function f() { return `t ${n}`; }\n  </script>\n</body>\n</html>\n",
	LangSvelte:     "<script lang=\"ts\">\n  import Child from './Child.svelte';\n  let { count = 0 }: { count?: number } = $props();\n  const doubled = $derived(count * 2);\n</script>\n\n{#if count > 0}\n  <Child value={doubled} on:click={() => count++} />\n{:else}\n  <p>Empty {count}</p>\n{/if}\n\n<style>\n  p { color: gray; }\n</style>\n",
	LangMarkdown:   "# Title\n\nSome *emphasis* and **strong** text with `code` and a [link](https://example.com).\n\n> quote line\n> continued\n\n- item one\n- item two\n  1. nested\n\n```go\nfunc main() {\n\tprintln(\"hi\")\n}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n",
	LangYAML:       "# config\nname: sample\non:\n  push:\n    branches: [main, 'release/*']\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: |\n          echo multi\n          echo line\n    env:\n      COUNT: 3\n      FLAG: true\n      anchor: &base {a: 1}\n      ref: *base\n",
	LangDiff:       "diff --git a/f.go b/f.go\nindex 123..456 100644\n--- a/f.go\n+++ b/f.go\n@@ -1,4 +1,5 @@\n package f\n-func old() {}\n+func new() {}\n+// added\n context line\n",
	LangSQL:        "-- comment\nCREATE TABLE items (\n  id INTEGER PRIMARY KEY,\n  name TEXT NOT NULL DEFAULT 'x',\n  created_at TIMESTAMP\n);\n/* block\n   comment */\nSELECT i.id, COUNT(*) AS n\nFROM items i\nLEFT JOIN tags t ON t.item_id = i.id\nWHERE i.name LIKE 'a%' AND i.id > 10\nGROUP BY i.id\nORDER BY n DESC;\n",
	LangRust:       "use std::collections::HashMap;\n\n/// Doc comment.\n#[derive(Debug, Clone)]\npub struct Item<'a> {\n    name: &'a str,\n    count: u32,\n}\n\nimpl<'a> Item<'a> {\n    pub fn new(name: &'a str) -> Self {\n        let raw = r#\"raw \"string\"\"#;\n        let _m: HashMap<String, i32> = HashMap::new();\n        println!(\"{} {}\", name, raw);\n        Self { name, count: 0 }\n    }\n}\n\nfn main() {\n    match Item::new(\"x\").count {\n        0 => {}\n        n if n > 1 => {}\n        _ => unreachable!(),\n    }\n}\n",
	LangC:          "#include <stdio.h>\n#define MAX(a, b) ((a) > (b) ? (a) : (b))\n\n/* block\n   comment */\ntypedef struct node {\n    int value;\n    struct node *next;\n} node_t;\n\nstatic int sum(const node_t *n) {\n    int total = 0;\n    for (; n != NULL; n = n->next) {\n        total += n->value;\n    }\n    return total; // done\n}\n\nint main(void) {\n    printf(\"%d\\n\", MAX(1, 2));\n    return 0;\n}\n",
	LangCPP:        "#include <vector>\nnamespace app {\ntemplate <typename T>\nclass Box {\npublic:\n    explicit Box(T value) : value_(std::move(value)) {}\n    auto get() const -> const T& { return value_; }\nprivate:\n    T value_;\n};\n}  // namespace app\n\nint main() {\n    auto v = std::vector<int>{1, 2, 3};\n    for (auto& x : v) { x *= 2; }\n    const char* raw = R\"(raw\nstring)\";\n    return static_cast<int>(v.size());\n}\n",
	LangJava:       "package dev.sample;\n\nimport java.util.List;\n\n/**\n * Javadoc.\n */\n@SuppressWarnings(\"unchecked\")\npublic final class Sample<T> implements Runnable {\n    private static final int MAX = 10;\n    private final List<T> items;\n\n    public Sample(List<T> items) {\n        this.items = items;\n    }\n\n    @Override\n    public void run() {\n        String text = \"\"\"\n            text block\n            \"\"\";\n        items.forEach(item -> System.out.println(item + text));\n    }\n}\n",
	LangTOML:       "# config\ntitle = \"sample\"\n\n[owner]\nname = 'Tom'\ndob = 1979-05-27T07:32:00-08:00\n\n[database]\nports = [ 8000, 8001 ]\nenabled = true\nmulti = \"\"\"\nline one\nline two\"\"\"\n\n[[servers]]\nip = \"10.0.0.1\"\n",
	LangINI:        "; comment\n[core]\n\teditor = vim\n\tautocrlf = false\n\n[remote \"origin\"]\n\turl = git@example.com:repo.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n# another comment\n[alias]\n\tst = status\n",
	LangHCL:        "# comment\nterraform {\n  required_version = \">= 1.5\"\n}\n\nvariable \"region\" {\n  type    = string\n  default = \"us-east-1\"\n}\n\nresource \"aws_instance\" \"web\" {\n  ami           = \"ami-123\"\n  instance_type = var.size\n  tags = {\n    Name = \"web-${var.region}\"\n  }\n  count = length(var.zones) > 0 ? 2 : 1\n}\n/* block\n   comment */\n",
	LangDockerfile: "# syntax=docker/dockerfile:1\nFROM golang:1.22 AS build\nARG VERSION=dev\nENV CGO_ENABLED=0 \\\n    GOOS=linux\nWORKDIR /src\nCOPY . .\nRUN go build -ldflags \"-X main.version=${VERSION}\" -o /out/app ./cmd/app \\\n    && strip /out/app\n\nFROM scratch\nCOPY --from=build /out/app /app\nEXPOSE 8080\nENTRYPOINT [\"/app\"]\n",
	LangMake:       "# comment\nGO ?= go\nSRC := $(wildcard *.go)\n\n.PHONY: build test\n\nbuild: $(SRC)\n\t$(GO) build -o bin/app ./...\n\ntest:\n\t@echo \"running tests\"\n\t$(GO) test ./... \\\n\t\t-count=1\n\nifeq ($(OS),Windows_NT)\nEXE := .exe\nendif\n",
	LangXML:        "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<!-- comment\n     spanning lines -->\n<manifest xmlns:android=\"http://schemas.android.com/apk/res/android\">\n  <application android:label=\"@string/app\" android:debuggable=\"true\">\n    <activity android:name=\".Main\">\n      <![CDATA[ raw <text> ]]>\n    </activity>\n  </application>\n</manifest>\n",
	LangPowerShell: "# comment\n<#\n  block comment\n#>\nparam(\n    [string]$Name = 'world',\n    [switch]$Loud\n)\nfunction Get-Greeting {\n    param([string]$Who)\n    $text = @\"\nHello $Who\n\"@\n    if ($Loud) { return $text.ToUpper() }\n    return $text\n}\nGet-ChildItem -Path . -Filter *.txt | ForEach-Object { $_.Name }\nWrite-Host (Get-Greeting -Who $Name)\n",
}

// TestStreamMatchesHighlight streams every sample in random chunks and checks
// that after each append the stream's lines equal a whole-document highlight
// of the same prefix, and that every line before the reported first changed
// line kept its previous spans.
func TestStreamMatchesHighlight(t *testing.T) {
	for lang := range langNames {
		if lang == LangPlaintext || lang == LangMarkdownInline {
			continue
		}
		if _, ok := streamSamples[lang]; !ok {
			t.Errorf("no stream sample for %s", lang)
		}
	}
	for lang, sample := range streamSamples {
		t.Run(lang.String(), func(t *testing.T) {
			src := []byte(sample)
			for seed := int64(0); seed < 4; seed++ {
				rng := rand.New(rand.NewSource(seed))
				s := NewStream(lang)
				var prev []EncodedLine
				for off := 0; off < len(src); {
					n := 1
					if seed > 0 {
						n = 1 + rng.Intn(int(seed)*12)
					}
					end := min(off+n, len(src))
					// Seed 3 drops the tree on alternate appends, which is the
					// path a stream takes once the tree budget is spent.
					keep := seed != 3 || rng.Intn(2) == 0
					from, state := s.Append(src[off:end], keep)
					off = end
					if state != StreamOK {
						t.Fatalf("seed %d: append to %d bytes: state %d", seed, off, state)
					}
					want := Highlight(lang, src[:off]).Lines
					got := s.Lines(0)
					if len(got) != len(want) {
						t.Fatalf("seed %d at %d bytes: %d lines, want %d", seed, off, len(got), len(want))
					}
					for i := range want {
						if !sameRuns(got[i], want[i]) {
							line := strings.Split(string(src[:off]), "\n")[i]
							t.Fatalf("seed %d at %d bytes: line %d %q runs %v, want %v", seed, off, i, line, got[i].Runs, want[i].Runs)
						}
					}
					for i := 0; i < from && i < len(prev); i++ {
						if !sameRuns(got[i], prev[i]) {
							t.Fatalf("seed %d at %d bytes: line %d changed but first changed line is %d", seed, off, i, from)
						}
					}
					if from < len(got) && from < len(prev) && sameRuns(got[from], prev[from]) {
						t.Fatalf("seed %d at %d bytes: first changed line %d did not change", seed, off, from)
					}
					prev = got
				}
				s.Close()
			}
		})
	}
}

func TestStreamPlainLanguageCountsLines(t *testing.T) {
	s := NewStream(LangPlaintext)
	if from, state := s.Append([]byte("a\nb"), true); state != StreamOK || from != 1 {
		t.Fatalf("first append: from %d state %d", from, state)
	}
	if from, _ := s.Append([]byte("c\n\nd"), true); from != 2 {
		t.Fatalf("second append: from %d, want 2", from)
	}
	if got := len(s.Lines(0)); got != 4 || s.LineCount() != 4 {
		t.Fatalf("lines %d count %d, want 4", got, s.LineCount())
	}
}

func TestStreamStopsAtInputCap(t *testing.T) {
	// The appends under the cap parse up to 1 MB; on a loaded host the
	// wall-clock parse deadline would turn them into StreamIncomplete.
	prev := parseTimeout
	parseTimeout = 0
	defer func() { parseTimeout = prev }()
	s := NewStream(LangGo)
	defer s.Close()
	line := []byte(strings.Repeat("x", 99) + "\n")
	chunk := bytes.Repeat(line, 1000)
	for len(s.Source())+len(chunk) <= maxInputBytes {
		if _, state := s.Append(chunk, true); state != StreamOK {
			t.Fatalf("under the cap: state %d", state)
		}
	}
	if !s.HoldsTree() {
		t.Fatal("under the cap: the stream kept no tree")
	}
	if _, state := s.Append(chunk, true); state != StreamOverCap {
		t.Fatalf("past the cap: state %d, want StreamOverCap", state)
	}
	if s.HoldsTree() {
		t.Fatal("an over-cap stream still holds its tree")
	}
	if _, state := s.Append([]byte("y"), true); state != StreamOverCap {
		t.Fatalf("after the cap: state %d, want StreamOverCap", state)
	}
}

func TestStreamCloseKeepsLinesAndReparses(t *testing.T) {
	src := streamSamples[LangGo]
	half := strings.Index(src, "func ")
	s := NewStream(LangGo)
	s.Append([]byte(src[:half]), true)
	if !s.HoldsTree() {
		t.Fatal("keepTree did not keep the tree")
	}
	s.Close()
	if s.HoldsTree() {
		t.Fatal("Close kept the tree")
	}
	s.Append([]byte(src[half:]), true)
	want := Highlight(LangGo, []byte(src)).Lines
	got := s.Lines(0)
	for i := range want {
		if !sameRuns(got[i], want[i]) {
			t.Fatalf("line %d runs %v, want %v", i, got[i].Runs, want[i].Runs)
		}
	}
	s.Close()
}

func TestStreamKeepsItsTreeAcrossATimedOutParse(t *testing.T) {
	prev := parseTimeout
	parseTimeout = 0
	defer func() { parseTimeout = prev }()
	src := streamSamples[LangGo]
	half := strings.Index(src, "func ")
	s := NewStream(LangGo)
	defer s.Close()
	s.Append([]byte(src[:half]), true)
	// A large append under a deadline no parse can meet stands in for a
	// loaded host.
	filler := strings.Repeat("// filler line\n", 2000)
	parseTimeout = time.Microsecond
	if _, state := s.Append([]byte(filler), true); state != StreamIncomplete {
		t.Fatalf("state %d under an expired deadline, want StreamIncomplete", state)
	}
	if !s.HoldsTree() {
		t.Fatal("a timed-out parse dropped the tree, so every later append reparses the whole document")
	}
	parseTimeout = 0
	if from, state := s.Append([]byte(src[half:]), true); state != StreamOK || from > strings.Count(src[:half], "\n") {
		t.Fatalf("after the retry: state %d from line %d, want StreamOK covering the lines the failed append added", state, from)
	}
	full := src[:half] + filler + src[half:]
	want := Highlight(LangGo, []byte(full)).Lines
	got := s.Lines(0)
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if !sameRuns(got[i], want[i]) {
			t.Fatalf("line %d runs %v, want %v", i, got[i].Runs, want[i].Runs)
		}
	}
}
