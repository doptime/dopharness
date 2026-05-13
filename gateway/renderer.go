package gateway

import (
	"fmt"
	"sort"
	"strings"

	"github.com/doptime/dopharness/chunk"
)

// Renderer 把决策和 chunk 数据组装成最终 prompt 片段。
//
// 输出按**文件**分组——所有同文件的 chunk 收纳在同一个 <file> 元素里,
// 按源码出现顺序排列。三态(FULL / SKELETON / IGNORE)以行内标记呈现,
// 让 LLM 看到的视觉结构最接近真实文件:
//
//	<file path="orch/agent.go" chunks="3" ignored="1">
//	[chunk xQrm FULL: struct EvolutionRequest]
//	type EvolutionRequest struct {
//	    TaskID string
//	    ...
//	}
//	[/chunk xQrm]
//
//	[chunk yK9p SKELETON: struct EvolutionReport]
//	type EvolutionReport struct
//	[/chunk yK9p]
//
//	[chunk aB12 IGNORED]
//	</file>
//
// 设计取舍:
//   - chunk 之间的 package / import / 顶层注释 字节不会出现(我们手上只有 chunk 列表,
//     没有原文件)。这是相比"行号方案"额外付出的一点信息丢失,换来的是:chunk_id 跨
//     session 稳定、不需要担心多次修改的行号漂移。
//   - 同文件如果有完全没出现在决策表里的 chunk(理论上不该发生 —— Selector 总会兜底
//     IGNORE),它们不会渲染。
type Renderer struct{}

// NewRenderer 构造默认 Renderer。
func NewRenderer() *Renderer {
	return &Renderer{}
}

// Render 把按文件分组的 chunk + decisions 组装成可粘进 prompt 的字符串。
//
// byFile 由 Gateway.BuildContext 用 store.ChunksByFile 取得,自然保持源码顺序。
// 顺序保证:文件按路径字典序排列,同文件内 chunk 按 byFile 给定的顺序(源码顺序)。
func (r *Renderer) Render(byFile map[string][]*chunk.Chunk, decisions DecisionMap) string {
	// 把文件路径排个序,产出稳定
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var sb strings.Builder
	totalFull, totalSkel, totalIgn := 0, 0, 0
	for _, p := range paths {
		f, s, i := r.renderFile(&sb, p, byFile[p], decisions)
		totalFull += f
		totalSkel += s
		totalIgn += i
		sb.WriteString("\n")
	}

	// 顶部摘要:让 LLM 一眼知道我们裁掉了多少
	header := fmt.Sprintf("<context files=\"%d\" full=\"%d\" skeleton=\"%d\" ignored=\"%d\">\n",
		len(paths), totalFull, totalSkel, totalIgn)
	footer := "</context>\n"
	return header + sb.String() + footer
}

// renderFile 渲染单个文件,返回该文件内三态的计数。
func (r *Renderer) renderFile(sb *strings.Builder, path string, chunks []*chunk.Chunk, decisions DecisionMap) (full, skel, ign int) {
	// 先盘点本文件三态计数
	for _, c := range chunks {
		switch modeOf(decisions, c.ID) {
		case ModeFull:
			full++
		case ModeSkeleton:
			skel++
		default:
			ign++
		}
	}

	fmt.Fprintf(sb, "<file path=%q chunks=\"%d\" full=\"%d\" skeleton=\"%d\" ignored=\"%d\">\n",
		path, len(chunks), full, skel, ign)

	for _, c := range chunks {
		switch modeOf(decisions, c.ID) {
		case ModeFull:
			r.writeFullChunk(sb, c)
		case ModeSkeleton:
			r.writeSkeletonChunk(sb, c)
		default:
			r.writeIgnoredChunk(sb, c)
		}
	}
	sb.WriteString("</file>\n")
	return
}

func (r *Renderer) writeFullChunk(sb *strings.Builder, c *chunk.Chunk) {
	fmt.Fprintf(sb, "[chunk %s FULL: %s %s]\n", c.ID, c.Kind, c.Name)
	sb.WriteString(c.Body)
	if len(c.Body) == 0 || c.Body[len(c.Body)-1] != '\n' {
		sb.WriteString("\n")
	}
	fmt.Fprintf(sb, "[/chunk %s]\n", c.ID)
}

func (r *Renderer) writeSkeletonChunk(sb *strings.Builder, c *chunk.Chunk) {
	sig := firstMeaningfulLine(c.Body)
	fmt.Fprintf(sb, "[chunk %s SKELETON: %s %s]\n", c.ID, c.Kind, c.Name)
	sb.WriteString(sig)
	if len(sig) == 0 || sig[len(sig)-1] != '\n' {
		sb.WriteString("\n")
	}
	fmt.Fprintf(sb, "[/chunk %s]\n", c.ID)
}

func (r *Renderer) writeIgnoredChunk(sb *strings.Builder, c *chunk.Chunk) {
	fmt.Fprintf(sb, "[chunk %s IGNORED]\n", c.ID)
}

// modeOf 安全取 decisions 里的 mode,未登记一律视为 IGNORE。
func modeOf(decisions DecisionMap, id string) Mode {
	if d, ok := decisions[id]; ok {
		return d.Mode
	}
	return ModeIgnore
}
