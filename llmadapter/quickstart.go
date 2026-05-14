// Package llmadapter — file quickstart.go
//
// QuickStart 把"用户最常需要的端到端 wiring"收纳到一个调用里。
//
// 它的目标只有一个:让那些不需要任何定制化的项目,在 5 行代码内拿到一个
// 完全可用的 Harness。
//
// 对比 (做同样的事):
//
//	旧手写:
//	  callers := buildCallers()                            // 50+ 行
//	  h, _ := harness.New(harness.Config{                 //
//	      ProjectRoot:  ".",                              //
//	      TriageCaller: callers.triage,                   //
//	      ExpandCaller: callers.expand,                   //
//	      MainCaller:   callers.main,                     //
//	  })                                                  //
//	  toolList := h.AsLLMTools(callers.toolBuilder)       //
//	  toolList = h.WithMemoryTools(callers.toolBuilder, toolList)
//
//	QuickStart:
//	  h, tools, _ := llmadapter.QuickStart(llmadapter.QuickConfig{
//	      ProjectRoot: ".",
//	      Model:       llm.Qwen36_35ba3b,
//	  })
//
// 仍然保留 Default / MultiModel / NewXxxCaller 作为低层入口 ——
// QuickStart 只是它们的薄壳。需要细粒度控制时直接用低层 API,
// 不需要时享受高层便利。
package llmadapter

import (
	"errors"
	"fmt"

	"github.com/doptime/llm"

	"github.com/doptime/dopharness/harness"
)

// QuickConfig 是 QuickStart 的配置入口。
//
// 必填:ProjectRoot,以及 Model 或 Models 之一。
// 其它字段都有合理默认。
type QuickConfig struct {
	// ProjectRoot 是项目根目录。必填。
	ProjectRoot string

	// Model 是简单的"所有角色都用同一个模型"配置。
	// 与 Models 互斥:如果两者都填了,以 Models 为准。
	Model *llm.Model

	// Models 是按角色分配模型的高级配置。
	// 至少需要 Models.Main 非空才视为有效配置。
	Models Models

	// DisableMemoryTools 关闭 task_reflect 工具的注册。
	// 默认开启 —— 这是 dopharness 作为"完备 Agent"的关键能力之一。
	DisableMemoryTools bool

	// DisableExpand 关闭 gateway Pass2 (skeleton→full 升级决策)。
	// 默认开启 (与 harness.Config.EnableExpand 的语义同步)。
	// 关闭 Pass2 节约一半 triage token,但可能丢失关键实现细节。
	DisableExpand bool

	// MaxRetries 覆盖 harness.Config.MaxRetries。0 → 默认 3。
	// 仅控制单次 Run 内的工具失败重试;RunTask 的外层 verify 重试由 Task.MaxRounds 控制。
	MaxRetries int

	// Logger 是 harness 的事件回调,nil 则不记录。
	Logger harness.Logger

	// TriageShardSize / TriageConcurrency 是 gateway Pass1 的并发参数。
	// 0 → 用 harness 的默认值 (50 / 8)。
	TriageShardSize   int
	TriageConcurrency int
}

// validate 检查必填字段。
func (c *QuickConfig) validate() error {
	if c.ProjectRoot == "" {
		return errors.New("llmadapter: QuickConfig.ProjectRoot is required")
	}
	if c.Model == nil && c.Models.Main == nil {
		return errors.New("llmadapter: QuickConfig requires either Model or Models.Main to be set")
	}
	return nil
}

// resolveModels 决定最终使用的 Models 配置。
// Models.Main 非空时优先;否则用 Model 单模型铺满。
func (c *QuickConfig) resolveModels() Models {
	if c.Models.Main != nil {
		return c.Models.resolve()
	}
	return Models{Main: c.Model}.resolve()
}

// QuickStart 一站式创建并配置好 Harness。
//
// 返回三元组:
//
//   - h:已构造好的 Harness,所有 caller 已注入,bundle 已构造完毕。
//     调用方只需要再调一次 h.Index(ctx),然后就可以 h.RunTask(...)。
//   - tools:LLM-side 的工具列表 (含 edit tools + task_reflect)。
//     调用方传给自己的 LLM agent (例如 doptime/llm 的 agent.UseTools)。
//     注意:tools 已经也被注册进了 h.bundle.Tools,所以 h.Run / h.RunTask
//     内部直接调 MainCaller 时也能看到所有工具。这里返回它主要是给那些
//     绕过 RunTask、自己跑 agent loop 的高阶用户用的。
//   - error:配置错误或 harness 初始化错误。
//
// QuickStart 不调用 h.Index() —— 索引是项目资产决策,留给调用方主动触发。
func QuickStart(cfg QuickConfig) (*harness.Harness, []any, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}

	models := cfg.resolveModels()
	cs := MultiModel(models)

	hCfg := harness.Config{
		ProjectRoot:       cfg.ProjectRoot,
		TriageCaller:      cs.Triage,
		ExpandCaller:      cs.Expand,
		MainCaller:        cs.Main,
		EnableExpand:      !cfg.DisableExpand,
		MaxRetries:        cfg.MaxRetries,
		TriageShardSize:   cfg.TriageShardSize,
		TriageConcurrency: cfg.TriageConcurrency,
		Logger:            cfg.Logger,
	}

	h, err := harness.New(hCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("llmadapter QuickStart: harness.New: %w", err)
	}

	// 注册 edit tools 到 bundle。
	toolList := h.AsLLMTools(cs.ToolBuilder)

	// 默认启用 memory tools (task_reflect),让 LLM 写内省自动入 L4。
	if !cfg.DisableMemoryTools {
		toolList = h.WithMemoryTools(cs.ToolBuilder, toolList)
	}

	return h, toolList, nil
}
