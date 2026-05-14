// Package harness — file memory_tools.go
//
// LLM 自主管理记忆的第一个原语:task_reflect 工具。
//
// 设计取舍 (回应用户的三大问题):
//
//   1. 为什么从 reflection 这一刀切入?
//      因为它最小、最安全、最能验证"LLM 可以管理记忆"的可行性。
//      它是 GenericAgent 自进化机制 ("crystallize execution path into skill")
//      的最小可工作单元。L4 归档现在不只是框架的外部观察,而是叠加了 LLM
//      对"为什么这么改、学到了什么"的内部解释。这两个视角合起来,才是
//      未来 L2 元认知层能学习的高质量语料。
//
//   2. 为什么不一次到位做 memory_read / memory_search?
//      读路径已经被 memory.Memory.Render 自动覆盖(L0-L4 全部默认渲染到
//      prompt)。在 L4 归档还没积累到几百条之前,显式 search 工具是过早
//      优化。先让自动写入跑稳,数据攒够,再决定 search 工具的形态。
//
//   3. 为什么 LLM 不能直接写 L2/L3?
//      L2 (Global Facts) 需要强模型维护 —— worker 模型对全局一致性的判断
//      不够可靠;L3 (Skills) 的形成需要跨任务比较,单次任务内的 LLM 看不
//      到全貌。这两层留给未来的"distiller"步骤(强模型在多次 L4 归档之上
//      做整理),不开放给主循环里的 worker。
//
// 实现要点:
//   - Opt-in:外部调用方必须显式调用 h.WithMemoryTools(...) 才注册工具
//   - Per-task scope:RunTask 自动安装新 recorder,defer 清理
//   - Graceful degradation:未启用时 RunTask 完全等价于上一刀
//   - 严格增量:通过包级 sync.Map 外挂 per-Harness 状态,无需修改 Harness struct
//   - 幂等:重复调用 WithMemoryTools 只注册一次
package harness

import (
	"strings"
	"sync"

	"github.com/doptime/dopharness/tools"
)

// TaskReflectPayload 是 LLM 通过 task_reflect 工具提交的内容。
//
// 字段设计:一条简短的"我学到了什么"足够。
//   - 不鼓励小作文 (LLM 易把 reflection 当成长篇大论)
//   - 不做结构化字段细分 (召回的语料天然是自由文本,LLM 自己组织信息密度)
//   - 允许多次调用,每次一条,LLM 自己决定"该总结的有几条"
type TaskReflectPayload struct {
	Insight string `json:"insight" jsonschema:"description=本次任务的简短内省总结(中文)。一次一条,可调用多次。建议每条围绕一个主题:解题思路、关键发现、可复用的模式、踩过的坑。任务很简单时不要调用此工具。"`
}

// reflectionRecorder 是 per-task 的内省收集器。
//
// 线程安全:LLM 在多 ToolCall 并发的实现里可能同时多次调用 task_reflect
// (虽然实际上多数 LLM 库串行调度,但稳妥起见做并发安全)。
type reflectionRecorder struct {
	mu       sync.Mutex
	insights []string
}

func newReflectionRecorder() *reflectionRecorder {
	return &reflectionRecorder{}
}

// add 追加一条 insight。空白被忽略,避免 LLM 偶发提交空字符串污染归档。
func (r *reflectionRecorder) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.insights = append(r.insights, s)
}

// snapshot 返回当前所有 insights 的浅拷贝,供归档使用。
// 没有 insight 时返回 nil (与空切片含义相同,但 JSON marshaler 会跳过 nil 字段)。
func (r *reflectionRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.insights) == 0 {
		return nil
	}
	out := make([]string, len(r.insights))
	copy(out, r.insights)
	return out
}

// ============================================================
// Per-Harness 外部状态
//
// 我们刻意不在 Harness struct 上加字段 (保持本次改动严格增量),
// 改用包级 sync.Map 把状态外挂在 *Harness 上。
//
// 取舍:
//   + 不破坏 harness/harness.go 的结构定义,降低本次合并的风险
//   + Harness 是个高聚合体,本来就不该为单一特性加字段
//   - 若 Harness 被 GC 时 map 中仍有 entry 会泄漏一个 recorder (几十字节)。
//     clearReflection 通过 defer 调用,正常路径不会泄漏;
//     只在极端情况 (panic 未被任何 recover 接住,进程也活着) 会有少量泄漏。
//     可接受。
// ============================================================

// reflectionRecorders 把 *Harness 映射到当前 active recorder。
// nil 表示该 Harness 当前不在任何 RunTask 里。
var reflectionRecorders sync.Map // map[*Harness]*reflectionRecorder

// memoryToolsInstalled 标记某 Harness 已注册过 memory tools,用于 WithMemoryTools 的幂等。
var memoryToolsInstalled sync.Map // map[*Harness]struct{}

// installReflection 给 Harness 安装一个新的 recorder。
// 由 RunTask 在 task 开始时调用;返回的 recorder 用于在归档时读取内容。
func (h *Harness) installReflection() *reflectionRecorder {
	r := newReflectionRecorder()
	reflectionRecorders.Store(h, r)
	return r
}

// clearReflection 清理 Harness 的 recorder。
// 由 RunTask 在 task 结束时通过 defer 调用。
// 即使工具未注册也安全调用 (Delete 是幂等的)。
func (h *Harness) clearReflection() {
	reflectionRecorders.Delete(h)
}

// currentReflection 返回当前 Harness 上活跃的 recorder。
//
// 返回 nil 的合法情形:
//   - 工具被 LLM 在 RunTask 外调用 (例如调用方手动调用 h.Run 而非 RunTask)
//   - task_reflect 工具未被注册却被 LLM 错误调用 (理论上不会发生,因为
//     LLM 看不到未注册的工具,但代码层面我们容错)
//
// 这两种情况下,工具的 handler 静默丢弃 payload,不影响主流程。
func (h *Harness) currentReflection() *reflectionRecorder {
	if v, ok := reflectionRecorders.Load(h); ok {
		return v.(*reflectionRecorder)
	}
	return nil
}

// ============================================================
// 对外 API
// ============================================================

// WithMemoryTools 把 LLM 可调用的记忆管理工具追加到 Harness 的工具 bundle。
//
// 当前提供:
//
//   - task_reflect:LLM 在任务结束时主动写"内省总结",并入 L4 归档。
//     LLM 看到这个工具时,应当在完成主要编辑后调用一次或几次,把
//     "解题思路 / 关键发现 / 可复用模式" 各写一条。
//
// 未来可能追加 (按数据驱动的需要):
//
//   - memory_search_sessions:当 L4 积累到几百条后,显式搜索过去会话
//   - memory_list_skills:在 L3 skills 数量超过一屏时,LLM 按名召回
//   - memory_read_skill:读单条 skill 的完整内容
//
// 调用方式 (典型):
//
//	baseTools := h.AsLLMTools(builder)
//	allTools  := h.WithMemoryTools(builder, baseTools)
//	// 把 allTools 配置进 LLM agent...
//	result, _ := h.RunTask(ctx, task)
//
// 副作用:
//
//   本方法会修改 h.bundle.Tools,使 h.Run / h.RunTask 内部直接调用
//   MainCaller 时 (它收到的是 h.bundle.Tools) 也能看到新增工具。
//   返回的 allTools 与 h.bundle.Tools 是同一个底层数组 (slice header 一致)。
//
// 幂等:
//
//   同一 Harness 多次调用只注册一次工具,后续调用直接返回当前 bundle.Tools。
//
// 前置条件:
//
//   必须已调用 h.AsLLMTools(builder) (即 bundle 已构造)。
//   否则方法返回 baseTools 原样,不报错 —— 留给上层调用方决定该如何处理。
//   这种"软失败"设计是因为 WithMemoryTools 是 opt-in 的便利方法,
//   不应替代正确的初始化顺序检查。
func (h *Harness) WithMemoryTools(builder tools.ToolBuilder, baseTools []any) []any {
	if h.bundle == nil {
		// bundle 还没构造 —— 调用方要么先调 AsLLMTools,要么不会用到 Run。
		// 优雅返回而非 panic/error,保持本方法的"扩展性"语义。
		return baseTools
	}
	// LoadOrStore 是幂等性的原子实现:第一次返回 false (没存过) 并写入,
	// 后续返回 true 直接走快速路径。避免重复 append 到 bundle.Tools。
	if _, already := memoryToolsInstalled.LoadOrStore(h, struct{}{}); already {
		return h.bundle.Tools
	}

	reflectTool := builder.Build(
		"task_reflect",
		"在任务结束时调用一次或多次,记录你对本次任务的内省总结(中文)。"+
			"每条 insight 围绕一个主题:解题思路、关键发现、可复用的模式或陷阱。"+
			"这些会并入 L4 会话归档,供未来类似任务自动召回。"+
			"任务很简单不需要总结时,不要调用本工具。",
		func(p *TaskReflectPayload) {
			// handler 在每次工具调用时执行:查当前活跃 recorder 并写入。
			// 这种"调用时查找"的设计让 tool 可以一次构造、跨多个 RunTask 复用。
			if rec := h.currentReflection(); rec != nil {
				rec.add(p.Insight)
			}
			// rec 为 nil 时静默丢弃,见 currentReflection 的注释。
		},
	)

	h.bundle.Tools = append(h.bundle.Tools, reflectTool)
	h.logEvent("memory_tools_registered", map[string]any{
		"added": []string{"task_reflect"},
	})
	return h.bundle.Tools
}
