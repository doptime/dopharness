// Package harness 是 dopharness 对外的最终门面。
//
// 一个 Harness 实例聚合了系统的 6 个子系统:
//   store    -> chunk 持久化
//   chunk    -> parser (Go + TS sidecar)
//   index    -> 扫描 + 增量
//   edit     -> 修改回写
//   gateway  -> 三态上下文网关
//   memory   -> L0-L4 分层记忆
//
// 用户无需 import 任何子包,只要:
//
//   h, _ := harness.New(harness.Config{...})
//   h.Index(ctx)
//   result, _ := h.Run(ctx, "给 extractGoFunc 加上 context 参数")
//
// 见 README.md 的完整示例。
package harness

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/edit"
	"github.com/doptime/dopharness/gateway"
	"github.com/doptime/dopharness/index"
	"github.com/doptime/dopharness/memory"
	"github.com/doptime/dopharness/store"
	"github.com/doptime/dopharness/tools"
)

// Config 是 Harness 的构造参数。
//
// 除 ProjectRoot 外,所有字段都可选。未提供的部分会用合理默认值。
type Config struct {
	// ProjectRoot 是项目根目录的绝对或相对路径。必填。
	ProjectRoot string

	// StoreDir 是持久化目录。默认 <ProjectRoot>/.dopharness。
	StoreDir string

	// MemoryRoot 是记忆层根目录。默认 <StoreDir>/memory。
	MemoryRoot string

	// GoConcurrency 是 Go 文件并行解析的并发度。0 = NumCPU。
	GoConcurrency int

	// TriageShardSize 是 Pass1 每片最多容纳多少个 chunk。默认 50。
	TriageShardSize int

	// TriageConcurrency 是 Pass1 并发请求数。默认 8。
	TriageConcurrency int

	// EnableExpand 控制是否启用 Pass2。默认 true(精度优先)。
	EnableExpand bool

	// TriageCaller 是 Pass1 LLM 调用器。必填(除非你只用 Index 和 BuildContext 前半段)。
	TriageCaller gateway.TriageCaller

	// ExpandCaller 是 Pass2 调用器。EnableExpand 为 true 时必填。
	ExpandCaller gateway.ExpandCaller

	// MainCaller 是最终修改轮的 LLM 调用器 —— 接收 system/user prompt + 工具列表,
	// 返回时 Collector 已经收集好 LLM 调的所有 ToolCall。
	// h.Run 需要它,h.BuildContext 不需要。
	MainCaller MainCaller

	// MaxRetries 是 Run 的最大重试轮数(含第一次)。默认 3。
	// 每一轮把"上一轮的工具执行摘要"塞给 LLM 重新尝试。
	MaxRetries int

	// Logger 是可选的事件回调,用于调试/观测。
	Logger Logger
}

// MainCaller 是 h.Run 里真正发给主模型的调用器。
//
// 参数:
//   systemPrompt: 从 Memory 里 L0/L2 汇总的 system 段(含 meta rules 和 global facts)
//   userPrompt:   Memory 的 user 段(含 <project_context>、skills、history)+ 用户原任务
//   tools:        要注册给 LLM 的工具列表(已包含 edit + inspect 七个工具)
//                 实现方需要把它们传给 llm.Agent.UseTools(tools...) —— 静态类型是 any,
//                 具体类型取决于所用 llm 库。
//
// 返回 nil 表示 LLM 调用成功(LLM 的 ToolCall 已通过工具闭包写入 Collector)。
// 返回 error 表示 LLM 调用本身失败(网络、超时等),Run 会据此决定是否继续重试。
type MainCaller func(systemPrompt, userPrompt string, tools []any) error

// Logger 是 Harness 发出观测事件的钩子。
// 每个事件一个名字 + 一组键值。可以用 slog / zap 封装。
type Logger func(event string, fields map[string]any)

// RunReport 汇总一次 Run 的全过程,便于外部观察/排错。
type RunReport struct {
	Rounds          int                    // 实际运行的轮次
	ContextReport   *gateway.SelectReport  // 最终那一轮的 gateway 报告(上下文裁定)
	ToolRecords     [][]*tools.Record      // 每一轮的工具执行记录
	Success         bool                   // 最终是否 AllOK
	LastError       error                  // 最后一次致命错误(若有)
}

// Harness 是门面主对象。
type Harness struct {
	cfg Config

	store    store.ChunkStore
	tsParser *chunk.TSParser
	indexer  *index.Indexer
	validator *edit.Validator
	applier  *edit.Applier
	gateway  *gateway.Gateway
	memory   *memory.Memory
	bundle   *tools.Bundle

	// 顶层锁:Index 与 Run 不能同时跑(索引可能正在改 store;Run 也会读 store)
	opMu sync.Mutex
}

// applyDefaults 填充默认配置。
func (c *Config) applyDefaults() error {
	if c.ProjectRoot == "" {
		return errors.New("harness: ProjectRoot is required")
	}
	abs, err := filepath.Abs(c.ProjectRoot)
	if err != nil {
		return fmt.Errorf("harness: resolve ProjectRoot: %w", err)
	}
	c.ProjectRoot = abs

	if c.StoreDir == "" {
		c.StoreDir = filepath.Join(c.ProjectRoot, ".dopharness")
	}
	if c.MemoryRoot == "" {
		c.MemoryRoot = filepath.Join(c.StoreDir, "memory")
	}
	if c.TriageShardSize <= 0 {
		c.TriageShardSize = 50
	}
	if c.TriageConcurrency <= 0 {
		c.TriageConcurrency = 8
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	return nil
}

// logEvent 是安全的日志调用(Logger 为 nil 时 no-op)。
func (h *Harness) logEvent(event string, fields map[string]any) {
	if h.cfg.Logger != nil {
		h.cfg.Logger(event, fields)
	}
}
