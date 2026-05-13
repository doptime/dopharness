package gateway

import (
	"errors"
	"sort"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// Gateway 是三态上下文网关的门面。
// 它把 store、selector、renderer 粘合起来,对外只暴露一个方法:BuildContext。
type Gateway struct {
	Store    store.ChunkStore
	Selector *Selector
	Renderer *Renderer
}

// New 构造 Gateway。
func New(s store.ChunkStore, sel *Selector, r *Renderer) (*Gateway, error) {
	if s == nil {
		return nil, errors.New("gateway: Store is required")
	}
	if sel == nil {
		return nil, errors.New("gateway: Selector is required")
	}
	if r == nil {
		r = NewRenderer()
	}
	return &Gateway{Store: s, Selector: sel, Renderer: r}, nil
}

// BuildResult 是 BuildContext 的返回值,除了最终字符串还带上决策细节,方便日志/调试。
type BuildResult struct {
	Prompt    string
	Decisions DecisionMap
	Report    *SelectReport
}

// BuildContext 是核心入口。输入用户任务描述,输出可直接拼到 LLM prompt 的文件视图字符串。
//
// 流程:
//  1. 从 store 拉全量 chunk
//  2. Selector 两 Pass 决策(Pass1 分片并发,Pass2 整批)
//  3. 按文件分组(走 ChunksByFile 保证源码顺序),交给 Renderer
func (g *Gateway) BuildContext(userPrompt string) (*BuildResult, error) {
	chunks := g.Store.AllChunks()
	decisions, report := g.Selector.Select(userPrompt, chunks)

	byFile := groupByFileSourceOrder(g.Store, chunks)
	prompt := g.Renderer.Render(byFile, decisions)
	return &BuildResult{
		Prompt:    prompt,
		Decisions: decisions,
		Report:    report,
	}, nil
}

// BuildContextFromChunks 是给单测/高级用户用的入口:绕过 store,直接给 chunk 列表。
// 由于没有 store 来取源码顺序,这里按 (FilePath, Name) 排序作为退化方案。
func (g *Gateway) BuildContextFromChunks(userPrompt string, chunks []*chunk.Chunk) *BuildResult {
	decisions, report := g.Selector.Select(userPrompt, chunks)
	byFile := groupByFileFallbackOrder(chunks)
	prompt := g.Renderer.Render(byFile, decisions)
	return &BuildResult{
		Prompt:    prompt,
		Decisions: decisions,
		Report:    report,
	}
}

// groupByFileSourceOrder 走 store.ChunksByFile,得到真实源码顺序的分组。
func groupByFileSourceOrder(s store.ChunkStore, chunks []*chunk.Chunk) map[string][]*chunk.Chunk {
	// 用 chunks 这个全量列表只是为了把"出现过哪些文件"挑出来——AllChunks 顺序无关紧要。
	paths := map[string]struct{}{}
	for _, c := range chunks {
		paths[c.FilePath] = struct{}{}
	}
	out := make(map[string][]*chunk.Chunk, len(paths))
	for p := range paths {
		out[p] = s.ChunksByFile(p) // 源码顺序
	}
	return out
}

// groupByFileFallbackOrder 当没有 store 时按 (FilePath, Name) 排,顺序虽不真但稳定。
func groupByFileFallbackOrder(chunks []*chunk.Chunk) map[string][]*chunk.Chunk {
	out := map[string][]*chunk.Chunk{}
	for _, c := range chunks {
		out[c.FilePath] = append(out[c.FilePath], c)
	}
	for _, cs := range out {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	}
	return out
}
