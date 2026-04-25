package gateway

import (
	"fmt"
	"sort"
	"strings"

	"github.com/doptime/dopharness/chunk"
)

// Renderer 把决策和 chunk 数据组装成最终 prompt 片段。
//
// 输出格式(三段):
//
//	<ignored_chunks count="N">
//	  <!-- 每行: <ignored id="xxxx" name="Foo" path="a.go"/> -->
//	</ignored_chunks>
//
//	<skeleton_chunks>
//	  <chunk id="xxxx" name="Foo" path="a.go" kind="Function">
//	  <skeleton 内容>
//	  </chunk>
//	  ...
//	</skeleton_chunks>
//
//	<full_chunks>
//	  <chunk id="xxxx" name="Foo" path="a.go" kind="Function">
//	  <完整 body>
//	  </chunk>
//	  ...
//	</full_chunks>
//
// 三段里都可能为空;空段仍然输出标签,让 LLM 知道"系统确实裁定这里没有东西"。
// 这比静默省略更不容易让 LLM 产生"是不是漏给我了?"的猜疑。
type Renderer struct {
	// IncludeIgnoredDetails 决定 ignored 段是只报数量还是也列出每个 chunk 的 name/path。
	// 对大项目(几千 chunk)列详情会膨胀 prompt,默认 false。
	IncludeIgnoredDetails bool
}

// NewRenderer 构造默认 Renderer。
func NewRenderer() *Renderer {
	return &Renderer{}
}

// Render 把 decisions + chunks 组装成可粘进 prompt 的字符串。
//
// 顺序保证:在同一段内,chunk 按 (FilePath, Name) 排序,让同一文件的 chunk 相邻,
// 便于 LLM 建立"这些是一起的"的上下文关联。
func (r *Renderer) Render(chunks []*chunk.Chunk, decisions DecisionMap) string {
	byID := map[string]*chunk.Chunk{}
	for _, c := range chunks {
		byID[c.ID] = c
	}

	var ignored, skeletonList, fullList []*chunk.Chunk
	for id, d := range decisions {
		c := byID[id]
		if c == nil {
			continue // 决策里提到了不存在的 ID,丢弃
		}
		switch d.Mode {
		case ModeIgnore:
			ignored = append(ignored, c)
		case ModeSkeleton:
			skeletonList = append(skeletonList, c)
		case ModeFull:
			fullList = append(fullList, c)
		}
	}

	sortChunks(ignored)
	sortChunks(skeletonList)
	sortChunks(fullList)

	var sb strings.Builder
	r.renderIgnored(&sb, ignored)
	sb.WriteString("\n")
	r.renderSkeletons(&sb, skeletonList)
	sb.WriteString("\n")
	r.renderFull(&sb, fullList)
	return sb.String()
}

func (r *Renderer) renderIgnored(sb *strings.Builder, chunks []*chunk.Chunk) {
	if !r.IncludeIgnoredDetails {
		fmt.Fprintf(sb, "<ignored_chunks count=\"%d\"/>\n", len(chunks))
		return
	}
	fmt.Fprintf(sb, "<ignored_chunks count=\"%d\">\n", len(chunks))
	for _, c := range chunks {
		fmt.Fprintf(sb, "  <ignored id=%q name=%q path=%q/>\n",
			c.ID, c.Name, c.FilePath)
	}
	sb.WriteString("</ignored_chunks>\n")
}

func (r *Renderer) renderSkeletons(sb *strings.Builder, chunks []*chunk.Chunk) {
	if len(chunks) == 0 {
		sb.WriteString("<skeleton_chunks count=\"0\"/>\n")
		return
	}
	fmt.Fprintf(sb, "<skeleton_chunks count=\"%d\">\n", len(chunks))
	for _, c := range chunks {
		r.writeChunkTag(sb, c, c.Skeleton)
	}
	sb.WriteString("</skeleton_chunks>\n")
}

func (r *Renderer) renderFull(sb *strings.Builder, chunks []*chunk.Chunk) {
	if len(chunks) == 0 {
		sb.WriteString("<full_chunks count=\"0\"/>\n")
		return
	}
	fmt.Fprintf(sb, "<full_chunks count=\"%d\">\n", len(chunks))
	for _, c := range chunks {
		r.writeChunkTag(sb, c, c.Body)
	}
	sb.WriteString("</full_chunks>\n")
}

// writeChunkTag 写一个 <chunk> 元素,体是 content。
// 体内会被 CDATA-like 的方式包含 —— 这里不做严格 XML escape,因为:
//  1. LLM 并不会真用 XML parser 解析我们的 prompt
//  2. 对源代码做 XML 转义反而会让 LLM 混淆
// 所以只要求 content 本身不包含我们的闭合标签即可;实际代码里遇到的概率极低。
func (r *Renderer) writeChunkTag(sb *strings.Builder, c *chunk.Chunk, content string) {
	fmt.Fprintf(sb, "<chunk id=%q name=%q path=%q kind=%q>\n",
		c.ID, c.Name, c.FilePath, string(c.Kind))
	sb.WriteString(content)
	// 确保闭合标签前有换行
	if len(content) == 0 || content[len(content)-1] != '\n' {
		sb.WriteString("\n")
	}
	sb.WriteString("</chunk>\n")
}

// sortChunks 按 (FilePath, Name) 升序。稳定排序不是必需,但方便 diff。
func sortChunks(cs []*chunk.Chunk) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].FilePath != cs[j].FilePath {
			return cs[i].FilePath < cs[j].FilePath
		}
		return cs[i].Name < cs[j].Name
	})
}
