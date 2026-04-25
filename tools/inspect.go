package tools

import (
	"fmt"
	"strings"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/edit"
)

// ReadChunkPayload 是 read_chunk 的参数。
type ReadChunkPayload struct {
	ChunkID string `json:"chunk_id" jsonschema:"description=要读取的 chunk 的 4 字符 ID"`
}

// SearchChunksByNamePayload 是 search_chunks_by_name 的参数。
type SearchChunksByNamePayload struct {
	Name string `json:"name" jsonschema:"description=要搜索的符号名(完全匹配,大小写敏感),例如 'Hello' 或 'User.Save'"`
}

// addInspectTools 把只读查询工具注册到 bundle。
// 这些工具不走 Applier,直接读 store,但出于"所有工具经过同一个 Collector"的一致性,
// 它们也会 Push 一条 Record(Result 的 Outcome 被填为 OutcomeApplied 表示成功,
// 或 OutcomeLocation 表示未找到)。
func addInspectTools(ap *edit.Applier, b ToolBuilder, bundle *Bundle) {
	store := ap.Store // Applier 依赖了 store,我们可以直接用

	// -- read_chunk --
	readHandler := func(p *ReadChunkPayload) {
		c, ok := store.GetChunk(strings.TrimSpace(p.ChunkID))
		rec := &Record{
			ToolName: "read_chunk",
			Request:  p,
		}
		if !ok {
			rec.Result = &edit.ApplyResult{
				Outcome: edit.OutcomeLocation,
				Message: fmt.Sprintf("chunk %q not found", p.ChunkID),
			}
		} else {
			rec.Result = &edit.ApplyResult{
				Outcome: edit.OutcomeApplied,
				Message: renderChunkForLLM(c),
				AffectedIDs: []string{c.ID},
			}
		}
		bundle.Collector.Push(rec)
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"read_chunk",
		"按 chunk_id 读取一个 chunk 的完整源码。用于在 modify_chunk 前先确认当前实现,或在定位失败后查看别的候选。",
		readHandler,
	))

	// -- search_chunks_by_name --
	searchHandler := func(p *SearchChunksByNamePayload) {
		matches := store.ChunksByName(strings.TrimSpace(p.Name))
		rec := &Record{
			ToolName: "search_chunks_by_name",
			Request:  p,
		}
		if len(matches) == 0 {
			rec.Result = &edit.ApplyResult{
				Outcome: edit.OutcomeLocation,
				Message: fmt.Sprintf("no chunk named %q", p.Name),
			}
		} else {
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			rec.Result = &edit.ApplyResult{
				Outcome:     edit.OutcomeApplied,
				Message:     renderMatchesForLLM(matches),
				AffectedIDs: ids,
			}
		}
		bundle.Collector.Push(rec)
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"search_chunks_by_name",
		"按名字搜索所有匹配的 chunk(大小写敏感,完全匹配)。返回它们的 ID、Kind、FilePath 列表。当 modify_chunk 因 ambiguous 失败时用这个来选正确的 ID。",
		searchHandler,
	))
}

// renderChunkForLLM 把 chunk 渲染成 LLM 友好的格式。
func renderChunkForLLM(c *chunk.Chunk) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "chunk id=%s kind=%s name=%s path=%s\n---\n",
		c.ID, c.Kind, c.Name, c.FilePath)
	sb.WriteString(c.Body)
	if !strings.HasSuffix(c.Body, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderMatchesForLLM 把多个候选渲染成一行一项的列表。
func renderMatchesForLLM(cs []*chunk.Chunk) string {
	lines := make([]string, 0, len(cs)+1)
	lines = append(lines, fmt.Sprintf("found %d matches:", len(cs)))
	for _, c := range cs {
		lines = append(lines, fmt.Sprintf("  id=%s kind=%s path=%s",
			c.ID, c.Kind, c.FilePath))
	}
	return strings.Join(lines, "\n")
}
