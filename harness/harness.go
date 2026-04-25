package harness

import (
	"context"
	"fmt"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/edit"
	"github.com/doptime/dopharness/gateway"
	"github.com/doptime/dopharness/index"
	"github.com/doptime/dopharness/memory"
	"github.com/doptime/dopharness/store"
	"github.com/doptime/dopharness/tools"
)

// New 构造并初始化一个 Harness。
//
// 行为:
//   - 自动创建 StoreDir 和 MemoryRoot
//   - 加载(或初始化空)store
//   - 构造所有子系统,但不自动执行索引 —— 调用方应随后调用 Index()
//   - 如果 TriageCaller 为 nil,gateway 会保持未配置,Run/BuildContext 将报错
func New(cfg Config) (*Harness, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	// store
	st := store.NewJSONStore(cfg.StoreDir)
	if err := st.Load(); err != nil {
		return nil, fmt.Errorf("harness: load store: %w", err)
	}

	// TS parser
	tsParser := chunk.NewTSParser()

	// indexer
	ix, err := index.NewIndexer(index.Config{
		ProjectRoot:   cfg.ProjectRoot,
		Store:         st,
		TSParser:      tsParser,
		GoConcurrency: cfg.GoConcurrency,
	})
	if err != nil {
		return nil, fmt.Errorf("harness: init indexer: %w", err)
	}

	// validator —— 通过函数适配器把 chunk.TSParser 接入 edit.Validator
	tsValidator := edit.TSValidatorFunc(func(code, kind string) (bool, int, int, string, error) {
		ok, errs, err := tsParser.ValidateCode(code, kind)
		if err != nil {
			return false, 0, 0, "", err
		}
		if ok {
			return true, 0, 0, "", nil
		}
		if len(errs) == 0 {
			return false, 0, 0, "ts validation failed", nil
		}
		return false, errs[0].Line, errs[0].Column, errs[0].Message, nil
	})
	validator := edit.NewValidator(tsValidator)

	// applier
	ap := edit.NewApplier(cfg.ProjectRoot, st, validator)
	ap.GoParse = chunk.ParseGoFile
	ap.TSParse = tsParser.ParseTSFile

	// gateway(只有在 caller 具备时才拉起;否则留 nil,Run/BuildContext 报错)
	var gw *gateway.Gateway
	if cfg.TriageCaller != nil {
		selCfg := gateway.SelectorConfig{
			ShardSize:    cfg.TriageShardSize,
			Concurrency:  cfg.TriageConcurrency,
			EnableExpand: cfg.EnableExpand,
			Triage:       cfg.TriageCaller,
			Expand:       cfg.ExpandCaller,
		}
		sel, err := gateway.NewSelector(selCfg)
		if err != nil {
			return nil, fmt.Errorf("harness: init selector: %w", err)
		}
		g, err := gateway.New(st, sel, nil)
		if err != nil {
			return nil, fmt.Errorf("harness: init gateway: %w", err)
		}
		gw = g
	}

	// memory(标准布局)
	mem := memory.BuildStandardMemory(memory.LayoutDirs{Root: cfg.MemoryRoot})

	h := &Harness{
		cfg:       cfg,
		store:     st,
		tsParser:  tsParser,
		indexer:   ix,
		validator: validator,
		applier:   ap,
		gateway:   gw,
		memory:    mem,
	}
	// bundle 先不构造,等 AsLLMTools 首次调用时再构造,这样对只用 Index 的用户零开销
	return h, nil
}

// Store 暴露底层 ChunkStore,便于调用方自己写查询/运维工具。
func (h *Harness) Store() store.ChunkStore { return h.store }

// Applier 暴露底层 Applier,允许绕过 Run 直接应用一批 Modification。
func (h *Harness) Applier() *edit.Applier { return h.applier }

// Memory 暴露底层 Memory,便于调用方写入 L4 记录或读 L3 技能。
func (h *Harness) Memory() *memory.Memory { return h.memory }

// Index 执行一次索引扫描。线程安全;与 Run 互斥。
//
// 索引结束后自动持久化 store(Flush)。
func (h *Harness) Index(ctx context.Context) (*index.Report, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()

	h.logEvent("index_start", nil)
	rep, err := h.indexer.Run(ctx)
	if err != nil {
		h.logEvent("index_error", map[string]any{"error": err.Error()})
		return rep, err
	}
	if flushErr := h.store.Flush(); flushErr != nil {
		h.logEvent("index_flush_error", map[string]any{"error": flushErr.Error()})
		return rep, fmt.Errorf("harness: flush store: %w", flushErr)
	}
	h.logEvent("index_done", map[string]any{
		"scanned": rep.FilesScanned,
		"indexed": rep.FilesIndexed,
		"skipped": rep.FilesSkipped,
		"removed": rep.FilesRemoved,
		"chunks":  rep.ChunksTotal,
	})
	return rep, nil
}

// BuildContextResult 是 BuildContext 的返回值。
// 它在 gateway.BuildResult 的基础上,把 memory 也一起算好。
type BuildContextResult struct {
	SystemPrompt string                // Memory 的 system 段
	UserPrompt   string                // Memory 的 user 段 + 用户原任务描述
	Decisions    gateway.DecisionMap   // gateway 的裁定
	Report       *gateway.SelectReport // gateway 的观测报告
	MemoryErrors []error               // memory 渲染出的非致命错误
}

// BuildContext 为一个用户任务生成完整的 system/user prompt。
//
// 这是 h.Run 的子步骤,也单独暴露出来,方便调用方自己跑 Agent Loop。
func (h *Harness) BuildContext(userPrompt string) (*BuildContextResult, error) {
	if h.gateway == nil {
		return nil, fmt.Errorf("harness: BuildContext requires TriageCaller in Config")
	}

	// 1. gateway 裁定
	gr, err := h.gateway.BuildContext(userPrompt)
	if err != nil {
		return nil, fmt.Errorf("harness: gateway: %w", err)
	}

	// 2. memory 组装
	sys, usr, memErrs := h.memory.Render(&memory.RenderCtx{
		UserPrompt:         userPrompt,
		ContextFromGateway: gr.Prompt,
	})

	// 3. 把用户任务作为 user prompt 的最后一段
	finalUser := usr
	if finalUser != "" {
		finalUser += "\n\n"
	}
	finalUser += "<user_task>\n" + userPrompt + "\n</user_task>"

	return &BuildContextResult{
		SystemPrompt: sys,
		UserPrompt:   finalUser,
		Decisions:    gr.Decisions,
		Report:       gr.Report,
		MemoryErrors: memErrs,
	}, nil
}

// ensureBundle 懒构造工具 bundle。
// Run 和 AsLLMTools 都会调用这里。
func (h *Harness) ensureBundle(b tools.ToolBuilder) *tools.Bundle {
	if h.bundle == nil {
		h.bundle = tools.BuildEditingTools(h.applier, b)
	}
	return h.bundle
}
