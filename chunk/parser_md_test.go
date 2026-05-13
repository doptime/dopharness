package chunk

import (
	"strings"
	"testing"
)

// TestMarkdown_BasicSections:基础场景,3 个 H2 节 → 3 个 Section chunk。
func TestMarkdown_BasicSections(t *testing.T) {
	src := `# Title

## Intro
Hello world.

## Goals
- A
- B

## Plan
Phase 1.
`
	chunks := parseMarkdownContent(src, "doc.md", 2)

	// 期望:1 个 Preamble(`# Title` 这行算 Preamble,因为 level=2 没把 H1 当切点)+ 3 个 Section
	if len(chunks) != 4 {
		t.Fatalf("want 4 chunks (preamble + 3 sections), got %d", len(chunks))
	}
	if chunks[0].Kind != KindPreamble {
		t.Errorf("chunks[0] should be Preamble, got %s", chunks[0].Kind)
	}
	for i, name := range []string{"Intro", "Goals", "Plan"} {
		c := chunks[i+1]
		if c.Kind != KindSection {
			t.Errorf("chunks[%d] should be Section, got %s", i+1, c.Kind)
		}
		if c.Name != name {
			t.Errorf("chunks[%d].Name = %q, want %q", i+1, c.Name, name)
		}
	}
}

// TestMarkdown_FencedCodeNotHeading:代码栅栏内的 # 不能被当成标题。
func TestMarkdown_FencedCodeNotHeading(t *testing.T) {
	src := "## Real\n```bash\n# this is a comment, not a heading\necho hi\n```\n## AlsoReal\n"
	chunks := parseMarkdownContent(src, "doc.md", 2)

	if len(chunks) != 2 {
		t.Fatalf("want 2 sections, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.Name == "this is a comment" {
			t.Fatalf("comment inside code fence was wrongly treated as heading")
		}
	}
	if chunks[0].Name != "Real" || chunks[1].Name != "AlsoReal" {
		t.Errorf("got names %q, %q", chunks[0].Name, chunks[1].Name)
	}
}

// TestMarkdown_Frontmatter:YAML frontmatter 应当独立成 chunk,不影响下方标题切分。
func TestMarkdown_Frontmatter(t *testing.T) {
	src := `---
title: Hello
tags: [a, b]
---
## First
Body.
`
	chunks := parseMarkdownContent(src, "doc.md", 2)

	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (frontmatter + section), got %d", len(chunks))
	}
	if chunks[0].Kind != KindFrontmatter {
		t.Errorf("chunks[0] should be Frontmatter, got %s", chunks[0].Kind)
	}
	if !strings.Contains(chunks[0].Body, "title: Hello") {
		t.Errorf("frontmatter body should include the YAML, got: %q", chunks[0].Body)
	}
	if chunks[1].Name != "First" {
		t.Errorf("section after frontmatter wrong name: %q", chunks[1].Name)
	}
}

// TestMarkdown_NoHeadings:无任何标题的文件,应当作为单个 Preamble chunk。
func TestMarkdown_NoHeadings(t *testing.T) {
	src := "Just some prose.\nNo headings at all.\n"
	chunks := parseMarkdownContent(src, "notes.md", 2)

	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Kind != KindPreamble {
		t.Errorf("kind should be Preamble, got %s", chunks[0].Kind)
	}
}

// TestMarkdown_DeepHeadingsStayInline:level=2 时,H3/H4 不独立成块,留在父级 H2 里。
func TestMarkdown_DeepHeadingsStayInline(t *testing.T) {
	src := `## Outer
intro

### Inner1
nested 1

### Inner2
nested 2

## NextOuter
`
	chunks := parseMarkdownContent(src, "doc.md", 2)

	if len(chunks) != 2 {
		t.Fatalf("want 2 sections (Outer, NextOuter), got %d", len(chunks))
	}
	outer := chunks[0]
	if outer.Name != "Outer" {
		t.Fatalf("first section name = %q, want Outer", outer.Name)
	}
	// Outer 的 body 应当包含两个 H3 子节
	if !strings.Contains(outer.Body, "### Inner1") || !strings.Contains(outer.Body, "### Inner2") {
		t.Errorf("Outer body should embed both H3 subsections, got:\n%s", outer.Body)
	}
}

// TestMarkdown_RefsExtraction:链接和 include 应当进 Refs。
func TestMarkdown_RefsExtraction(t *testing.T) {
	src := `## S
See [other doc](other.md) and [[shared_skill]] for more.
External: https://example.com
Anchor: [back to top](#top)
`
	chunks := parseMarkdownContent(src, "doc.md", 2)
	if len(chunks) != 1 {
		t.Fatalf("want 1 section, got %d", len(chunks))
	}
	refs := chunks[0].Refs
	wantRefs := map[string]bool{"other.md": false, "shared_skill": false}
	for _, r := range refs {
		if _, ok := wantRefs[r]; ok {
			wantRefs[r] = true
		}
	}
	for r, found := range wantRefs {
		if !found {
			t.Errorf("expected ref %q not found in %v", r, refs)
		}
	}
	// URL 和 anchor 不应该出现
	for _, r := range refs {
		if strings.HasPrefix(r, "http") || strings.HasPrefix(r, "#") {
			t.Errorf("ref %q should have been filtered out", r)
		}
	}
}

// TestMarkdown_LevelGranularity:把 level 设为 3 时,H3 也独立成块。
func TestMarkdown_LevelGranularity(t *testing.T) {
	src := `## Outer

### Sub1
content 1

### Sub2
content 2
`
	// level=2:Outer 是一个 chunk,Sub1/Sub2 嵌在内
	chunksL2 := parseMarkdownContent(src, "doc.md", 2)
	if len(chunksL2) != 1 {
		t.Errorf("level=2 want 1 chunk, got %d", len(chunksL2))
	}

	// level=3:Outer + Sub1 + Sub2 = 3 个 chunk
	chunksL3 := parseMarkdownContent(src, "doc.md", 3)
	if len(chunksL3) != 3 {
		t.Errorf("level=3 want 3 chunks, got %d", len(chunksL3))
	}
}

// TestMarkdown_EmptyFile:完全空的文件不产生 chunk(避免存空东西)。
func TestMarkdown_EmptyFile(t *testing.T) {
	chunks := parseMarkdownContent("", "empty.md", 2)
	if len(chunks) != 0 {
		t.Errorf("empty file should produce 0 chunks, got %d", len(chunks))
	}
	chunks = parseMarkdownContent("   \n  \n", "blank.md", 2)
	if len(chunks) != 0 {
		t.Errorf("whitespace-only file should produce 0 chunks, got %d", len(chunks))
	}
}

// TestMarkdown_StableBodyOnReparse:同样输入应产出同样的 chunk Body(确定性保证)。
func TestMarkdown_StableBodyOnReparse(t *testing.T) {
	src := "## A\nbody\n## B\nbody\n"
	first := parseMarkdownContent(src, "doc.md", 2)
	second := parseMarkdownContent(src, "doc.md", 2)
	if len(first) != len(second) {
		t.Fatalf("non-deterministic chunk count")
	}
	for i := range first {
		if first[i].Body != second[i].Body {
			t.Errorf("chunk %d body drifted between parses:\n%q\nvs\n%q",
				i, first[i].Body, second[i].Body)
		}
	}
}
