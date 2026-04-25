package gateway

import (
	"errors"

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

// BuildContext 是核心入口。输入用户任务描述,输出可直接拼到 LLM prompt 的三态字符串。
//
// 流程:
//  1. 从 store 拉全量 chunk
//  2. Selector 两 Pass 决策(Pass1 分片并发,Pass2 整批)
//  3. Renderer 按决策输出三态 XML 段
func (g *Gateway) BuildContext(userPrompt string) (*BuildResult, error) {
	chunks := g.Store.AllChunks()
	decisions, report := g.Selector.Select(userPrompt, chunks)
	prompt := g.Renderer.Render(chunks, decisions)
	return &BuildResult{
		Prompt:    prompt,
		Decisions: decisions,
		Report:    report,
	}, nil
}

// BuildContextFromChunks 是给单测/高级用户用的入口:绕过 store,直接给 chunk 列表。
func (g *Gateway) BuildContextFromChunks(userPrompt string, chunks []*chunk.Chunk) *BuildResult {
	decisions, report := g.Selector.Select(userPrompt, chunks)
	prompt := g.Renderer.Render(chunks, decisions)
	return &BuildResult{
		Prompt:    prompt,
		Decisions: decisions,
		Report:    report,
	}
}
