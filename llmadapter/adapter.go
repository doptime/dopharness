//	llmadapter wires github.com/doptime/llm into dopharness's caller
//
// interfaces with sensible defaults.
//
// 这个包是为了解决用户的核心抱怨:每个项目都要重写
// doptimeTriageCaller / doptimeMainCaller / doptimeToolBuilder
// 这三段几乎相同的样板代码。
//
// dopharness 的核心包 (gateway / harness / tools) 故意不 import doptime/llm,
// 保持 LLM 库无关 —— 这是结构上的正确选择,但代价是每个使用者要做一次桥接。
// llmadapter 就是官方桥接:
//
//   - 完整覆盖 dopharness 现有的所有工具 payload 类型
//   - 提供"一行启用所有 caller"的 Default(model) 入口
//   - 提供"按角色分配不同模型"的 MultiModel(models) 入口
//   - 允许逐个替换 (NewTriageCaller / NewMainCaller / NewToolBuilder)
//
// 如果使用者用的不是 doptime/llm 而是别的库 (e.g. anthropic-go),
// 他们可以参考本包的实现写自己的 adapter,核心包不需要改动。
package llmadapter

import (
	"fmt"
	"text/template"

	"github.com/doptime/llm"

	"github.com/doptime/dopharness/gateway"
	"github.com/doptime/dopharness/harness"
	"github.com/doptime/dopharness/tools"
)

// Models 把不同 agent 角色用的模型分开。
//
// 最简用法:只填 Main,其余角色自动 fallback。
// 成本敏感用法:Triage 用便宜小模型 (只输出结构化裁决),Main 用强模型 (写代码)。
type Models struct {
	// Main 是主编辑轮使用的模型。必填。
	Main *llm.Model

	// Triage 是 gateway Pass1 使用的模型(对每个 chunk 输出 IGNORE/SKELETON/FULL)。
	// 空值 fallback 到 Main。
	Triage *llm.Model

	// Expand 是 gateway Pass2 使用的模型(把 SKELETON 升级为 FULL 的二次决策)。
	// 空值 fallback 到 Triage(它本身 fallback 到 Main)。
	Expand *llm.Model
}

// resolve 把 Models 的零值字段按 fallback 链填满,返回一个完全确定的副本。
//
// fallback 链: Expand → Triage → Main
//
// 暴露在内部以便测试。
func (m Models) resolve() Models {
	if m.Triage == nil {
		m.Triage = m.Main
	}
	if m.Expand == nil {
		m.Expand = m.Triage
	}
	return m
}

// CallerSet 把 dopharness 需要的所有 LLM 侧组件聚成一个值。
//
// 它的字段恰好对应 harness.Config 里需要外部注入的几个回调,以及
// h.AsLLMTools 需要的 ToolBuilder。典型用法:
//
//	cs := llmadapter.Default(llm.Qwen36_35ba3b)
//	h, _ := harness.New(harness.Config{
//	    ProjectRoot:  ".",
//	    TriageCaller: cs.Triage,
//	    ExpandCaller: cs.Expand,  // 可选;EnableExpand=true 时必填
//	    MainCaller:   cs.Main,
//	})
//	toolList := h.AsLLMTools(cs.ToolBuilder)
//	toolList = h.WithMemoryTools(cs.ToolBuilder, toolList)
type CallerSet struct {
	Triage      gateway.TriageCaller
	Expand      gateway.ExpandCaller
	Main        harness.MainCaller
	ToolBuilder tools.ToolBuilder
}

// Default 用同一个模型构造一个全功能 CallerSet。
//
// 这是最常见的开发期用法 —— 一个模型 + 默认 prompt 模板,开箱即用。
func Default(model *llm.Model) *CallerSet {
	return MultiModel(Models{Main: model})
}

// MultiModel 按角色分配模型构造 CallerSet。
//
// 至少需要填 Main;其余按 fallback 链补全。
func MultiModel(m Models) *CallerSet {
	m = m.resolve()
	return &CallerSet{
		Triage:      NewTriageCaller(m.Triage, DefaultTriageTemplate),
		Expand:      NewExpandCaller(m.Expand, DefaultExpandTemplate),
		Main:        NewMainCaller(m.Main),
		ToolBuilder: NewToolBuilder(),
	}
}

// ============================================================
// 单个 caller 工厂
// ============================================================

// DefaultTriageTemplate 是 Triage Pass1 的项目无关模板。
//
// 设计原则:
//   - 不引入领域术语 (不假设"Go 项目"或"Markdown 项目")
//   - 用 chunk 的 Kind / Name / Signature / File / Refs 作为唯一线索
//   - 提示词指明"必须覆盖所有 chunk",防止 LLM 漏判
//   - 引导 LLM 把"被引用 / 定义类型 / 包含将要修改函数所在文件" 标 SKELETON,
//     把"即将被修改 / 直接相关"标 FULL,其余标 IGNORE
//
// 如果项目有特殊需求 (例如:文档项目只关心 .md 章节),用 NewTriageCaller
// 传入自定义模板即可覆盖。
const DefaultTriageTemplate = `你是 dopharness 的上下文过滤助手。
下面给出用户任务和候选 chunk 摘要,你需要为每个 chunk 在 IGNORE / SKELETON / FULL
中选一种,并通过 TriageDecision 工具一次性提交所有裁决。

裁决标准:
- FULL:即将被修改的 chunk,或与任务直接相关、需要看完整实现的 chunk
- SKELETON:被相关 chunk 引用、定义关键类型/接口、或提供必要背景的 chunk(只看签名即可)
- IGNORE:与本次任务完全无关

用户任务:
{{.UserPrompt}}

候选 chunk (id | kind | name | file | signature):
{{range .Views}}{{.ID}} | {{.Kind}} | {{.Name}} | {{.FilePath}} | {{.Signature}}
{{end}}

必须对每一个 chunk 做出裁决,不能遗漏。不确定时偏向 SKELETON 而不是 IGNORE。`

// DefaultExpandTemplate 是 gateway Pass2 (skeleton→full 升级决策) 的项目无关模板。
//
// Pass2 看到的是 Pass1 选为 SKELETON 的全部 chunk 的合并视图,要决定哪些
// 实际上应该升级为 FULL(因为它们承载了关键实现细节)。
// 大多数情况下不需要升级 (LLM 应保守);只在 SKELETON 的签名无法表达足够
// 信息时才升级。
const DefaultExpandTemplate = `你是 dopharness 的 Pass2 上下文细化助手。
Pass1 已经把下面这些 chunk 标记为 SKELETON。现在请你判断:它们之中哪些应当
**升级为 FULL** —— 即把完整源码也放进上下文。

升级标准:仅当 skeleton 签名行不足以理解该 chunk 在本次任务里的作用,且
完整源码包含关键决策信息(算法、状态机、复杂条件)时,才升级。
保守优先:大多数情况下不需要升级任何 chunk。

用户任务:
{{.UserPrompt}}

候选 (Pass1 已标 SKELETON 的) chunk:
{{range .Skeletons}}{{.ID}} | {{.Kind}} | {{.Name}} | {{.FilePath}} | {{.Signature}}
{{end}}

通过 ExpandDecision 工具提交要升级为 FULL 的 chunk_id 列表。可以为空。`

// NewTriageCaller 用指定模型 + 自定义模板构造一个 TriageCaller。
//
// 模板必须接受 .UserPrompt (string) 和 .Views ([]*gateway.ChunkView) 两个字段。
// 如果不需要自定义,传入 DefaultTriageTemplate。
func NewTriageCaller(model *llm.Model, promptTemplate string) gateway.TriageCaller {
	tpl := template.Must(template.New("triage").Parse(promptTemplate))
	return func(params gateway.TriagePromptParams, sink func(*gateway.TriageDecisionPayload)) error {
		var payload *gateway.TriageDecisionPayload
		tool := llm.NewTool(
			"TriageDecision",
			"提交对每个候选 chunk 的 IGNORE / SKELETON / FULL 裁决。必须覆盖所有输入 chunk。",
			func(p *gateway.TriageDecisionPayload) {
				payload = p
			},
		)
		agent := llm.NewAgent(tpl, tool).UseModels(model)
		if err := agent.Call(map[string]any{
			"UserPrompt": params.UserPrompt,
			"Views":      params.Views,
		}); err != nil {
			return fmt.Errorf("llmadapter triage: %w", err)
		}
		if payload == nil {
			return fmt.Errorf("llmadapter triage: LLM did not call TriageDecision tool")
		}
		sink(payload)
		return nil
	}
}

// NewExpandCaller 用指定模型 + 自定义模板构造一个 ExpandCaller (Pass2)。
//
// 与 Triage 同样的契约:模板取 .UserPrompt + .Skeletons。
func NewExpandCaller(model *llm.Model, promptTemplate string) gateway.ExpandCaller {
	tpl := template.Must(template.New("expand").Parse(promptTemplate))
	return func(params gateway.ExpandPromptParams, sink func(*gateway.ExpandDecisionPayload)) error {
		var payload *gateway.ExpandDecisionPayload
		tool := llm.NewTool(
			"ExpandDecision",
			"提交要从 SKELETON 升级为 FULL 的 chunk_id 列表。可以为空。",
			func(p *gateway.ExpandDecisionPayload) {
				payload = p
			},
		)
		agent := llm.NewAgent(tpl, tool).UseModels(model)
		if err := agent.Call(map[string]any{
			"UserPrompt": params.UserPrompt,
			"Skeletons":  params.Skeletons,
		}); err != nil {
			return fmt.Errorf("llmadapter expand: %w", err)
		}
		// Expand 允许 LLM 直接不调工具(等价于 0 项升级);此时 payload 仍为 nil,
		// 我们合成一个空载荷喂给 sink,保持 sink 在所有路径上都被调用一次的契约。
		if payload == nil {
			payload = &gateway.ExpandDecisionPayload{}
		}
		sink(payload)
		return nil
	}
}

// NewMainCaller 用指定模型构造一个 MainCaller (编辑轮)。
//
// systemPrompt 与 userPrompt 用两段拼接,中间空两行。这是因为 doptime/llm 的
// 当前 agent.Call API 接受单一模板渲染,没有单独的 system 通道;真要严格分
// 通道,需要等 doptime/llm 加 UseSystem 之类的方法,届时本函数可在内部升级
// 而不影响外部调用方。
func NewMainCaller(model *llm.Model) harness.MainCaller {
	tpl := template.Must(template.New("main").Parse(`{{.Prompt}}`))
	return func(systemPrompt, userPrompt string, toolList []any) error {
		// dopharness 用 any 表达 ToolInterface,这里要转回去
		llmTools := make([]llm.ToolInterface, 0, len(toolList))
		for i, t := range toolList {
			it, ok := t.(llm.ToolInterface)
			if !ok {
				return fmt.Errorf("llmadapter main: tools[%d] is %T, not llm.ToolInterface "+
					"(make sure you used NewToolBuilder to construct it)", i, t)
			}
			llmTools = append(llmTools, it)
		}

		agent := llm.NewAgent(tpl).UseModels(model).UseTools(llmTools...)
		combined := joinPrompts(systemPrompt, userPrompt)
		if err := agent.Call(map[string]any{"Prompt": combined}); err != nil {
			return fmt.Errorf("llmadapter main: %w", err)
		}
		return nil
	}
}

// joinPrompts 把 system 与 user 段合并;空段会被跳过,避免出现两段空行。
func joinPrompts(system, user string) string {
	switch {
	case system == "":
		return user
	case user == "":
		return system
	default:
		return system + "\n\n" + user
	}
}

// NewToolBuilder 返回一个 ToolBuilder,把 dopharness 已知的所有 payload 类型
// 转换成 llm.ToolInterface。
//
// 覆盖范围:
//   - tools.ModifyChunkPayload / DeleteChunkPayload / AddChunkPayload /
//     CreateFilePayload / DeleteFilePayload   (BuildEditingTools 用到的 5 种)
//   - harness.TaskReflectPayload   (WithMemoryTools 注册的 1 种)
//
// 后续若新增带新 payload 类型的工具,需要同步在这里加 case。
// 未覆盖类型会被快速 panic,这是早期失败 (fail-fast) 的有意设计 ——
// 让"忘记注册 case"的错误立即暴露,而不是默默地把工具丢失给 LLM。
func NewToolBuilder() tools.ToolBuilder {
	return tools.ToolBuilderFunc(func(name, desc string, handler any) any {
		switch h := handler.(type) {
		case func(*tools.ModifyChunkPayload):
			return llm.NewTool(name, desc, h)
		case func(*tools.DeleteChunkPayload):
			return llm.NewTool(name, desc, h)
		case func(*tools.AddChunkPayload):
			return llm.NewTool(name, desc, h)
		case func(*tools.CreateFilePayload):
			return llm.NewTool(name, desc, h)
		case func(*tools.DeleteFilePayload):
			return llm.NewTool(name, desc, h)
		case func(*harness.TaskReflectPayload):
			return llm.NewTool(name, desc, h)
		default:
			// 故意 panic 而不是 silent ignore:
			// 工具丢失而 LLM 没工具可调,会让上层用"AllOK 但没编辑"的假阳性
			// 状态继续走,远比"启动时立刻挂掉"难排查。
			panic(fmt.Sprintf("llmadapter: unknown tool handler type %T "+
				"for tool %q. Add a case to NewToolBuilder.", handler, name))
		}
	})
}
