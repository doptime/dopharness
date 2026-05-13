package chunk

// parser_md.go 实现 markdown 文件的确定性 AST 切片。
//
// 设计要点:
//   - 切片粒度由 MarkdownChunkLevel 控制(默认 2 = H2 节级)。每个 H<level> 标题
//     连同其正文形成一个 chunk,正文延伸到下一个**同级或更高级**标题为止。
//   - 比 level 更深的子标题(H3、H4...)留在父级 chunk 的 Body 内,不独立成块。
//     这样不会出现 chunk 文本重叠,也匹配人类编辑 markdown 的直觉(通常按 H2 节
//     整段改写,而不是单独改 H3 子段)。
//   - YAML frontmatter(文件首部 `---...---` 块)独立成 chunk(Kind=Frontmatter)。
//   - 第一个标题之前的内容(若非空)成为 Preamble chunk,name=preamble。
//   - 完全无标题的文件:整体作为单个 Preamble chunk。
//
// 不使用 LLM 拆分 markdown:LLM 拆分会让每次 indexing 产生不同的边界(随机决策),
// 导致 chunk ID 漂移,破坏整个 dopharness 引用稳定性。markdown 有天然 AST(ATX 标题
// 层级),写确定性 parser 即可。indexer.go 调度此 parser 的方式与 ParseGoFile 完全
// 一致 —— 接入只需在 indexer 的 dispatch 表里追加一个扩展名匹配。

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// MarkdownChunkLevel 是默认的切片粒度。
//
//	1 → 按 H1 切(整篇文章 = 1 chunk,适合多文档)
//	2 → 按 H2 切(默认,适合大多数 README / 设计文档)
//	3 → 按 H3 切(细粒度,适合长篇手册)
//
// 想动态调整时,直接改这个变量(全局生效)。
var MarkdownChunkLevel = 2

// ParseMarkdownFile 接口签名与 ParseGoFile 一致,可以直接挂进 indexer 的 dispatch 表。
func ParseMarkdownFile(absPath, relPath string) ([]*Chunk, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("chunk: read %s: %w", absPath, err)
	}
	return parseMarkdownContent(string(content), relPath, MarkdownChunkLevel), nil
}

// parseMarkdownContent 是核心切片逻辑,与磁盘 IO 解耦,便于单测。
func parseMarkdownContent(content, relPath string, level int) []*Chunk {
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	lines := strings.Split(content, "\n")

	// 1. 检测 frontmatter:首行 `---` + 找第二个 `---`
	frontmatterEnd := -1
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				frontmatterEnd = i
				break
			}
		}
	}

	// 2. 扫所有标题。**关键**:必须跳过代码栅栏内的 # 行(那不是标题,是代码内容)。
	inFence := false
	var headings []headingPos
	for i, line := range lines {
		if i <= frontmatterEnd {
			continue
		}
		ltrim := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(ltrim, "```") || strings.HasPrefix(ltrim, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if isH, lvl, name := parseATXHeading(line); isH && lvl <= level {
			headings = append(headings, headingPos{
				line:  i,
				level: lvl,
				name:  name,
			})
		}
	}

	var chunks []*Chunk

	// 3. frontmatter chunk
	if frontmatterEnd > 0 {
		body := strings.Join(lines[:frontmatterEnd+1], "\n")
		chunks = append(chunks, &Chunk{
			FilePath: relPath,
			Kind:     KindFrontmatter,
			Name:     "frontmatter",
			Body:     body,
		})
	}

	// 4. preamble:frontmatter 结束(或文件起始)到第一个标题之间
	preambleStart := frontmatterEnd + 1
	firstHeadingLine := len(lines)
	if len(headings) > 0 {
		firstHeadingLine = headings[0].line
	}
	if firstHeadingLine > preambleStart {
		body := strings.Join(lines[preambleStart:firstHeadingLine], "\n")
		if strings.TrimSpace(body) != "" {
			chunks = append(chunks, &Chunk{
				FilePath: relPath,
				Kind:     KindPreamble,
				Name:     "preamble",
				Body:     body,
				Refs:     extractMarkdownRefs(body),
			})
		}
	}

	// 5. 每个 heading -> 一个 Section chunk(包含其下深层子节内容)
	for i, h := range headings {
		endLine := len(lines)
		if i+1 < len(headings) {
			endLine = headings[i+1].line
		}
		body := strings.Join(lines[h.line:endLine], "\n")
		chunks = append(chunks, &Chunk{
			FilePath: relPath,
			Kind:     KindSection,
			Name:     h.name,
			Body:     body,
			Refs:     extractMarkdownRefs(body),
		})
	}

	// 6. 兜底:完全无结构的文件(整篇散文,无任何标题、无 frontmatter)
	if len(chunks) == 0 {
		body := strings.TrimSpace(content)
		if body != "" {
			chunks = append(chunks, &Chunk{
				FilePath: relPath,
				Kind:     KindPreamble,
				Name:     "content",
				Body:     body,
				Refs:     extractMarkdownRefs(body),
			})
		}
	}

	return chunks
}

// headingPos 是 parser 内部记录每个被识别标题位置的小结构。
type headingPos struct {
	line  int    // 在 lines 数组里的下标
	level int    // 1..6
	name  string // 标题文本(已去除 # 和首尾空格)
}

// atxHeadingRE 匹配 ATX 风格标题。
//
// 标准:1-6 个 # + 至少一个空格 + 标题文本 + 可选的尾部 # + 可选空格。
// 不支持 setext(下划线 === / ---)风格 —— 那种风格在工程文档里几乎绝迹,且
// 与 frontmatter 的 --- 视觉上冲突,加进来得不偿失。
var atxHeadingRE = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

func parseATXHeading(line string) (bool, int, string) {
	m := atxHeadingRE.FindStringSubmatch(strings.TrimRight(line, " \t"))
	if m == nil {
		return false, 0, ""
	}
	return true, len(m[1]), strings.TrimSpace(m[2])
}

// markdownRefRE 抓取所有形如 [text](url) 的链接、image ![](url)、以及
// {{include foo}} 这种模板包含语法的目标名。这些会作为 Refs 反向索引,
// 让 dopharness 的"被本 chunk 引用的符号 -> 应当 SKELETON 展示"逻辑能跨语言生效。
var markdownRefRE = regexp.MustCompile(`!?\[[^\]]+\]\(([^)\s]+)`)

// includeRE 匹配 {{include name}} / {{>name}} / [[name]] 等常见模板包含语法。
// 不同 SSG 用不同语法;v1 只覆盖最常见的几种。
var includeRE = regexp.MustCompile(`(?:\{\{\s*(?:include|>)\s+|\[\[)([^\}\]\s]+)`)

func extractMarkdownRefs(body string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		// 过滤纯 URL(http/https/mailto/锚点),只留可能是符号名的引用
		if s == "" {
			return
		}
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
			strings.HasPrefix(s, "mailto:") || strings.HasPrefix(s, "#") {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, m := range markdownRefRE.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	for _, m := range includeRE.FindAllStringSubmatch(body, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	return out
}
