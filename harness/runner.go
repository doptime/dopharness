package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/doptime/dopharness/tools"
)

// AsLLMTools 返回可以直接传给 llm.Agent.UseTools 的工具列表。
//
// builder 参数应当是一个把 llm.NewTool 桥接过来的 ToolBuilder。
// 典型实现:
//
//   builder := tools.ToolBuilderFunc(func(name, desc string, handler any) any {
//       return llm.NewTool(name, desc, handler)
//   })
//   toolList := h.AsLLMTools(builder)
//
// 返回的切片元素类型是 any,静态类型是 llm.ToolInterface 的实现;调用方需在传入
// UseTools 时做类型转换(或直接用 variadic 展开)。
func (h *Harness) AsLLMTools(builder tools.ToolBuilder) []any {
	return h.ensureBundle(builder).Tools
}

// Run 是一站式调用。流程(方案 B:自动重试):
//
//   1. BuildContext(userPrompt)  得到 system/user prompt
//   2. 第 1 轮:调用 MainCaller,LLM 通过 ToolCall 触发工具,Collector 记录结果
//   3. 如果 Collector.AllOK,返回成功
//   4. 否则,把上一轮的失败摘要追加进 user prompt,进入第 2 轮,依此至 MaxRetries
//   5. MaxRetries 轮后仍有失败 —— 返回最后一轮的 report,Success=false
//
// Run 总是会重新 BuildContext(只做一次):裁定在第一轮后不再变更。
// 这避免"LLM 改了代码 → 重跑 gateway → 上下文变了 → LLM 再次被骚扰"的震荡。
func (h *Harness) Run(ctx context.Context, userPrompt string) (*RunReport, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()

	if h.cfg.MainCaller == nil {
		return nil, fmt.Errorf("harness: Run requires MainCaller in Config")
	}

	// 构造上下文 + 工具(工具懒构造)
	bctx, err := h.BuildContext(userPrompt)
	if err != nil {
		return nil, err
	}

	// Run 需要一个 ToolBuilder —— 但我们不知道具体 LLM 库。
	// 约定:MainCaller 被调用时,Harness 会把 h.bundle.Tools 作为第三参传过去;
	// 而 bundle 只有在 AsLLMTools 被调用过一次后才存在。
	// 所以:如果用户没先 AsLLMTools 就直接 Run,我们报错说明原因。
	if h.bundle == nil {
		return nil, fmt.Errorf(
			"harness: Run requires prior call to AsLLMTools(builder) to register tool factory")
	}

	report := &RunReport{
		ContextReport: bctx.Report,
	}

	userPromptEvolving := bctx.UserPrompt

	for round := 1; round <= h.cfg.MaxRetries; round++ {
		if err := ctx.Err(); err != nil {
			report.LastError = err
			return report, err
		}
		h.logEvent("run_round_start", map[string]any{"round": round})

		h.bundle.Collector.Reset()
		err := h.cfg.MainCaller(bctx.SystemPrompt, userPromptEvolving, h.bundle.Tools)
		report.Rounds = round
		// 即使 MainCaller 返回 error,也收集 Records —— 可能部分工具已经跑过了
		roundRecords := append([]*tools.Record{}, h.bundle.Collector.Records()...)
		report.ToolRecords = append(report.ToolRecords, roundRecords)

		if err != nil {
			h.logEvent("run_llm_error", map[string]any{"round": round, "error": err.Error()})
			report.LastError = err
			// LLM 调用本身失败(网络等):不进入下一轮,直接返回
			return report, err
		}

		// LLM 啥工具都没调 —— 我们也不知道它要什么。v1 视作"本轮完成"直接返回。
		if len(roundRecords) == 0 {
			h.logEvent("run_round_no_tools", map[string]any{"round": round})
			report.Success = true // 没被要求做事,不算失败
			return report, nil
		}

		// 成功:所有工具都 OK
		if h.bundle.Collector.AllOK() {
			h.logEvent("run_success", map[string]any{"round": round, "ops": len(roundRecords)})
			report.Success = true
			return report, nil
		}

		// 失败且还有重试余地:把错误摘要追加到 user prompt
		if round < h.cfg.MaxRetries {
			summary := h.bundle.Collector.BuildSummary()
			userPromptEvolving = appendRetryFeedback(userPromptEvolving, round, summary)
			h.logEvent("run_retry", map[string]any{
				"round":        round,
				"next_round":   round + 1,
				"failed_calls": countFailed(roundRecords),
			})
		} else {
			h.logEvent("run_exhausted", map[string]any{
				"round":        round,
				"failed_calls": countFailed(roundRecords),
			})
		}
	}

	// 轮数耗尽,仍未 AllOK
	report.Success = false
	return report, nil
}

// appendRetryFeedback 把"上一轮的失败"以一种 LLM 能理解的格式追加到 prompt 尾部。
//
// 注意:我们不删除之前的 <user_task>,而是在其后补一段 <previous_attempt>。
// 这能让 LLM 同时看到"要做什么"和"上次做错了什么",便于自纠。
func appendRetryFeedback(current string, round int, summary string) string {
	var sb strings.Builder
	sb.WriteString(current)
	sb.WriteString("\n\n<previous_attempt round=\"")
	fmt.Fprintf(&sb, "%d", round)
	sb.WriteString("\">\n")
	sb.WriteString("Your previous tool calls had failures. Review the outcomes below, then retry. ")
	sb.WriteString("Do not repeat tool calls that already succeeded.\n")
	sb.WriteString(strings.TrimRight(summary, "\n"))
	sb.WriteString("\n</previous_attempt>")
	return sb.String()
}

func countFailed(records []*tools.Record) int {
	n := 0
	for _, r := range records {
		if !r.OK() {
			n++
		}
	}
	return n
}
