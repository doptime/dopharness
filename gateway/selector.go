package gateway

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/doptime/dopharness/chunk"
)

// TriageDecisionPayload 是 Pass1 LLM 必须通过 ToolCall 返回的结构。
// 字段 tag 用的是 github.com/doptime/llm 风格的 jsonschema。
type TriageDecisionPayload struct {
	Decisions []TriageItem `json:"decisions" jsonschema:"description=对每个候选 chunk 的裁决;必须覆盖所有输入 chunk"`
}

// TriageItem 是 Pass1 对单个 chunk 的裁决。
type TriageItem struct {
	ChunkID string `json:"chunk_id" jsonschema:"description=4 字符的 chunk ID,必须来自输入列表"`
	Mode    string `json:"mode" jsonschema:"enum=IGNORE,enum=SKELETON,enum=FULL,description=IGNORE=该chunk与任务完全无关,从上下文剔除;SKELETON=可能相关但不需要看实现,只给签名;FULL=直接相关,提供完整源码"`
	Reason  string `json:"reason,omitempty" jsonschema:"description=一句话说明理由,用于调试"`
}

// ExpandDecisionPayload 是 Pass2 的 ToolCall 结构。
// Pass2 只做"把哪些当前 SKELETON 的 chunk 升级为 FULL"的决策。
type ExpandDecisionPayload struct {
	Upgrades []ExpandItem `json:"upgrades" jsonschema:"description=决定升级为 FULL 的 chunk 列表;可以为空,表示不需要升级"`
}

// ExpandItem 是 Pass2 的一项升级决策。
type ExpandItem struct {
	ChunkID string `json:"chunk_id" jsonschema:"description=要升级为 FULL 的 chunk ID"`
	Reason  string `json:"reason,omitempty"`
}

// ----------------------------------------------------------------
// LLM 调用的抽象接口。实际实现桥接到 github.com/doptime/llm 的 Agent.Call。
// ----------------------------------------------------------------

// TriageCaller 是单次 Pass1 分片调用的契约。
// gateway 不直接 import doptime/llm,而是通过这个回调注入,保持解耦也便于单测。
//
// 实现应当:
//  1. 用 params 里的 UserPrompt 和 Views 渲染 prompt
//  2. 调 LLM,让它通过 ToolCall 返回一份 TriageDecisionPayload
//  3. 将结果通过 sink 回调丢回 gateway(可以多次调用,最后一次生效)
type TriageCaller func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error

// ExpandCaller 是 Pass2 的调用契约。
type ExpandCaller func(params ExpandPromptParams, sink func(*ExpandDecisionPayload)) error

// TriagePromptParams 是一次 Pass1 分片的输入。
type TriagePromptParams struct {
	UserPrompt string       // 用户任务原文
	Views      []*ChunkView // 本分片的 chunk 视图列表
}

// ExpandPromptParams 是一次 Pass2 的输入。
type ExpandPromptParams struct {
	UserPrompt string
	// Skeletons 是 Pass1 选为 SKELETON 的 chunk 的 skeleton 字符串(比 View 更详细)。
	Skeletons []*SkeletonItem
}

// SkeletonItem 是 Pass2 输入里的单项。
type SkeletonItem struct {
	ID        string
	Kind      chunk.Kind
	Name      string
	FilePath  string
	Signature string // chunk Body 的首条非注释行(签名/类型声明行)
}

// ----------------------------------------------------------------
// Selector 主体
// ----------------------------------------------------------------

// SelectorConfig 控制 Selector 行为。
type SelectorConfig struct {
	// ShardSize 是 Pass1 单次 LLM 调用的 chunk 数量上限。
	// 太大:prompt 过长,LLM 精度下降;太小:分片多,调用次数多。
	// 对本地 LLM,50 是一个平衡点。
	ShardSize int

	// Concurrency 是 Pass1 分片的并发度。本地 LLM 可以拉高到 8-16。
	Concurrency int

	// EnableExpand 控制是否执行 Pass2。false 时直接使用 Pass1 结果。
	EnableExpand bool

	// Triage 是 Pass1 的调用器(必填)。
	Triage TriageCaller

	// Expand 是 Pass2 的调用器(EnableExpand 为 true 时必填)。
	Expand ExpandCaller
}

// Selector 执行两 Pass 裁定。
type Selector struct {
	cfg SelectorConfig
}

// NewSelector 构造。基本配置检验。
func NewSelector(cfg SelectorConfig) (*Selector, error) {
	if cfg.Triage == nil {
		return nil, errors.New("gateway: Triage caller is required")
	}
	if cfg.EnableExpand && cfg.Expand == nil {
		return nil, errors.New("gateway: Expand caller required when EnableExpand=true")
	}
	if cfg.ShardSize <= 0 {
		cfg.ShardSize = 50
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	return &Selector{cfg: cfg}, nil
}

// SelectReport 汇总一次 Select 运行的情况,便于调试和观测。
type SelectReport struct {
	TotalChunks     int
	ShardsCount     int
	TriageErrors    []error
	ExpandErrored   bool
	ExpandError     error
	FullCount       int
	SkeletonCount   int
	IgnoreCount     int
	MissingCoverage []string // Pass1 未覆盖的 chunk ID(LLM 遗漏);会被默认设为 IGNORE
}

// Select 是主入口。输入:用户 prompt + 所有 chunk。输出:决策表 + 报告。
//
// 行为:
//   - 返回的 DecisionMap 保证覆盖所有输入 chunk(缺省为 IGNORE)
//   - 单个分片失败不中断整个流程,错误汇到 report.TriageErrors
//   - Pass2 失败也不致命,只是放弃升级,保留 Pass1 结果
func (s *Selector) Select(userPrompt string, chunks []*chunk.Chunk) (DecisionMap, *SelectReport) {
	report := &SelectReport{TotalChunks: len(chunks)}
	decisions := DecisionMap{}

	if len(chunks) == 0 {
		return decisions, report
	}

	// Pass 1: 分片 + 并发
	shards := shardChunks(chunks, s.cfg.ShardSize)
	report.ShardsCount = len(shards)

	pass1 := s.runPass1(userPrompt, shards, report)
	decisions.Merge(pass1)

	// 填默认 IGNORE
	for _, c := range chunks {
		if _, ok := decisions[c.ID]; !ok {
			report.MissingCoverage = append(report.MissingCoverage, c.ID)
			decisions[c.ID] = &Decision{
				ChunkID: c.ID,
				Mode:    ModeIgnore,
				Reason:  "not covered by triage pass",
			}
		}
	}

	// Pass 2: 整批升级
	if s.cfg.EnableExpand {
		skeletonChunks := collectSkeletonChunks(chunks, decisions)
		if len(skeletonChunks) > 0 {
			pass2, err := s.runPass2(userPrompt, skeletonChunks)
			if err != nil {
				report.ExpandErrored = true
				report.ExpandError = err
			} else {
				decisions.Merge(pass2)
			}
		}
	}

	// 计数
	for _, d := range decisions {
		switch d.Mode {
		case ModeFull:
			report.FullCount++
		case ModeSkeleton:
			report.SkeletonCount++
		default:
			report.IgnoreCount++
		}
	}

	return decisions, report
}

// ----------------------------------------------------------------
// Pass 1 并发实现
// ----------------------------------------------------------------

func (s *Selector) runPass1(userPrompt string, shards [][]*chunk.Chunk, report *SelectReport) DecisionMap {
	type shardResult struct {
		idx     int
		payload *TriageDecisionPayload
		err     error
	}

	results := make(chan shardResult, len(shards))
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup

	for i, shard := range shards {
		wg.Add(1)
		go func(idx int, shard []*chunk.Chunk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			views := make([]*ChunkView, len(shard))
			for j, c := range shard {
				views[j] = ToView(c)
			}
			params := TriagePromptParams{
				UserPrompt: userPrompt,
				Views:      views,
			}

			var received *TriageDecisionPayload
			sink := func(p *TriageDecisionPayload) {
				received = p
			}
			err := s.cfg.Triage(params, sink)
			results <- shardResult{idx: idx, payload: received, err: err}
		}(i, shard)
	}

	wg.Wait()
	close(results)

	// 收集并合并
	merged := DecisionMap{}
	for r := range results {
		if r.err != nil {
			report.TriageErrors = append(report.TriageErrors,
				fmt.Errorf("shard %d: %w", r.idx, r.err))
			continue
		}
		if r.payload == nil {
			report.TriageErrors = append(report.TriageErrors,
				fmt.Errorf("shard %d: no payload returned (LLM did not call tool)", r.idx))
			continue
		}
		for _, item := range r.payload.Decisions {
			mode := parseMode(item.Mode)
			// 同一 chunk 如果在多个分片里都出现(不会发生,但防御一下),按规则升级
			cur, exists := merged[item.ChunkID]
			newDec := &Decision{ChunkID: item.ChunkID, Mode: mode, Reason: item.Reason}
			if !exists || modeRank(mode) > modeRank(cur.Mode) {
				merged[item.ChunkID] = newDec
			}
		}
	}
	return merged
}

// ----------------------------------------------------------------
// Pass 2 整批实现
// ----------------------------------------------------------------

func (s *Selector) runPass2(userPrompt string, skeletons []*SkeletonItem) (DecisionMap, error) {
	params := ExpandPromptParams{
		UserPrompt: userPrompt,
		Skeletons:  skeletons,
	}
	var received *ExpandDecisionPayload
	sink := func(p *ExpandDecisionPayload) {
		received = p
	}
	err := s.cfg.Expand(params, sink)
	if err != nil {
		return nil, err
	}
	out := DecisionMap{}
	if received == nil {
		// LLM 没调 tool —— 视作"不升级",不算错误
		return out, nil
	}
	for _, u := range received.Upgrades {
		out[u.ChunkID] = &Decision{
			ChunkID: u.ChunkID,
			Mode:    ModeFull,
			Reason:  u.Reason,
		}
	}
	return out, nil
}

// ----------------------------------------------------------------
// 辅助
// ----------------------------------------------------------------

// shardChunks 把 chunk 列表切成每片至多 size 个的分片。
// 不做粗排序:根据你的指示"和以前一样,全量 chunk 带符号引用"。
func shardChunks(chunks []*chunk.Chunk, size int) [][]*chunk.Chunk {
	if size <= 0 {
		size = 50
	}
	out := make([][]*chunk.Chunk, 0, (len(chunks)+size-1)/size)
	for i := 0; i < len(chunks); i += size {
		end := i + size
		if end > len(chunks) {
			end = len(chunks)
		}
		out = append(out, chunks[i:end])
	}
	return out
}

// collectSkeletonChunks 根据 decisions 筛出需要 Pass2 审视的 chunk。
func collectSkeletonChunks(chunks []*chunk.Chunk, decisions DecisionMap) []*SkeletonItem {
	items := make([]*SkeletonItem, 0)
	for _, c := range chunks {
		if d, ok := decisions[c.ID]; ok && d.Mode == ModeSkeleton {
			items = append(items, &SkeletonItem{
				ID:        c.ID,
				Kind:      c.Kind,
				Name:      c.Name,
				FilePath:  c.FilePath,
				Signature: firstMeaningfulLine(c.Body),
			})
		}
	}
	return items
}

func parseMode(raw string) Mode {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "FULL":
		return ModeFull
	case "SKELETON":
		return ModeSkeleton
	case "IGNORE":
		return ModeIgnore
	}
	return ModeIgnore
}
