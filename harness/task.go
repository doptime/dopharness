// Package harness — file task.go
//
// 在 Harness.Run 之上引入 Task / RunTask —— 把 dopharness 从"工具包"提升为
// "完备的 Agent":
//
//	Run(userPrompt)  → 一次 LLM 编辑轮,内部带工具失败重试
//	RunTask(task)    → 由外部 Verify 驱动的外层循环,反复调用 Run,
//	                   并在结束时自动归档到 L4 (memory.SessionRecordsLayer)
//
// 这是把记忆系统从"等外部 Golang 程序按约定调用"转向"任务结束时自动写入"
// 的关键一步。配合 memory.SessionRecordsLayer.Render 已有的读路径,
// L4 归档现在能形成完整的读-写自动闭环 —— 下一次任务自动看到上一次的结果,
// 调用方无需关心记忆的细节。
//
// 设计要点 (与回答用户三大问题的对应):
//   1. 外部接口极简化:Task 只有一个必填字段 (Description),其余皆有合理默认;
//      不再要求调用方拼 prompt、跑循环、手写归档。
//   2. 验收驱动迭代:VerifyFunc 是唯一的"是否完成"裁定器,被调用方用任意手段
//      实现(跑测试、调审计、问真人都行)。MetricResult 提供可比的结构化维度,
//      为未来 L2 元认知层(强模型分析"什么 dimension 重要")提供原料。
//   3. 重试两层分离:Run 内部按工具失败重试;RunTask 按外部 Verify 失败重试。
//      职责清晰,不会互相干扰。
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/doptime/dopharness/memory"
)

// VerifyStatus 是单个命名指标的结果分类。
//
// 故意只用四个桶 —— 越多 LLM 越难判断"我是不是该停"。需要更细粒度时,
// 用 MetricResult.Detail 文本字段承载。
type VerifyStatus string

const (
	VerifyPass    VerifyStatus = "PASS"
	VerifyFail    VerifyStatus = "FAIL"
	VerifyPartial VerifyStatus = "PARTIAL"
	VerifyInfra   VerifyStatus = "INFRA_FAIL" // Verify 自身崩溃,不是被测物失败
)

// MetricResult 是 Verify 产出的"一个维度的命名结论"。
//
// 调用方按需返回零个或多个 MetricResult:
//   - 0 个 → 整体通过仅看 VerifyResult.Passed
//   - >=1 个 → 整体通过 ⇔ 全部 MetricResult.Status == VerifyPass
//     (此时 VerifyResult.Passed 被忽略,语义更明确)
//
// Detail 是给 LLM 看的简短说明 (例:"score=72, expected >=80"),不是给人看的长文。
type MetricResult struct {
	Name   string       `json:"name"`
	Status VerifyStatus `json:"status"`
	Detail string       `json:"detail,omitempty"`
}

// VerifyResult 是一次外部验收的结构化产物。
//
// 三个职责:
//  1. 决定是否还要继续迭代 (整体 Passed)
//  2. 给下一轮 LLM 提供结构化反馈 (Metrics)
//  3. 写入 L4 归档 (整体记录到 SessionRecord.Summary,可被未来召回)
//
// Diag 是自由文本,描述"哪里错了、为什么错"。它会被原样喂回下一轮 LLM,
// 因此请用 LLM 友好的格式 —— 简短、聚焦、可行动。避免堆栈和无关日志。
type VerifyResult struct {
	Passed  bool
	Metrics []MetricResult
	Diag    string
}

// VerifyFunc 是外部"做完了吗?"回调。
//
// 调用时机:
//   - 一次在任务开始 (round 0):判断是否根本不需要 LLM 介入
//   - 每一轮成功的 LLM 编辑之后:检查进展
//
// 实现必须对当前磁盘状态是确定性的:连续两次调用 (中间没有编辑)
// 应返回相同结果。否则 RunTask 的判停逻辑会不可靠。
type VerifyFunc func(ctx context.Context) (VerifyResult, error)

// Task 是一次完整 Agent 运行的高层单位。
//
// 对外只有一个必填字段 (Description);其余皆有合理默认。
//
// 典型用法:
//
//	result, err := h.RunTask(ctx, harness.Task{
//	    Description: "修复 coffee-cloze 游戏的计分 bug",
//	    Verify: func(ctx context.Context) (harness.VerifyResult, error) {
//	        return runAudit(ctx)  // 调用方自己定义"完成"的判定
//	    },
//	    MaxRounds: 3,
//	    BetweenRounds: func(ctx context.Context, round int) error {
//	        time.Sleep(4 * time.Second)  // 等 HMR 重启
//	        return nil
//	    },
//	    ArchiveTag: "fix-scoring",
//	})
type Task struct {
	// Description 是自然语言任务描述。
	// 每一轮都会以 <user_task> 包装后送入 LLM。
	// 必填。
	Description string

	// Verify 是外部验收回调。
	//
	// nil → RunTask 等价于一次 h.Run,只看工具调用是否成功;
	// 非 nil → RunTask 会循环至验收通过或耗尽 MaxRounds。
	Verify VerifyFunc

	// MaxRounds 限制 verify 驱动的外层循环最大次数。
	// 0 → DefaultMaxRounds。
	// 超过仍未通过时,RunTask 返回最后一次验收的 verdict,
	// 不返回 error (error 仅用于"流程崩溃"如 ctx 取消、verify panic 等)。
	MaxRounds int

	// BetweenRounds 可选:每轮编辑结束、下一次 Verify 开始前调用。
	// 用途:等待 HMR 重启、重新部署、外部 worker 重启、休眠避免限流。
	// 返回 error 会终止 RunTask 并归档为 "hook-error"。
	BetweenRounds func(ctx context.Context, round int) error

	// ArchiveTag 是 L4 归档 ID 的前缀 (可选)。
	// 一个项目里跑多种任务时,用 tag 把它们分开,便于日后召回检索。
	ArchiveTag string
}

// DefaultMaxRounds 是未指定 MaxRounds 时的默认值。
const DefaultMaxRounds = 3

// TaskResult 是 RunTask 的产物。
//
// 字段语义:
//   - Passed:任务最终是否通过 (以最后一次 Verify 为准;无 Verify 时以 Run.Success 为准)
//   - Rounds:实际执行的外层循环轮数 (0 表示初始 Verify 已通过)
//   - Metrics / LastDiag:最后一次 Verify 的结构化与文本反馈
//   - RunReports:每一轮 Run 的细节报告 (便于排查)
//   - ArchiveID:L4 归档落盘后的记录 ID;若归档失败,留空
//   - Reflections:LLM 通过 task_reflect 工具主动提交的内省总结。
//     仅当调用方启用了 WithMemoryTools 且 LLM 确实调用了该工具时非空。
//     这是"框架外部视角"与"LLM 内部视角"在 L4 归档里的合并点。
type TaskResult struct {
	Passed      bool
	Rounds      int
	Metrics     []MetricResult
	LastDiag    string
	RunReports  []*RunReport
	ArchiveID   string
	Reflections []string
}

// RunTask 是把 Task 跑完的统一入口。
//
// 流程:
//
//	round 0:    Verify? → 已通过则直接返回
//	round k:    Run(description + 历次反馈) → BetweenRounds → Verify
//	重复直到通过或耗尽 MaxRounds
//	收尾:      不论成败,都写一条 L4 SessionRecord (best-effort)
//
// 错误处理三态:
//   - 任务正常跑完 (无论 Passed 与否) → err == nil
//   - 流程崩溃 (ctx 取消、Verify 自身报错、LLM 调用报错、BetweenRounds 报错)
//     → 同时返回部分填充的 TaskResult 和 err,并尝试归档
//
// 调用前置条件:
//   - Harness 必须已调用 AsLLMTools(builder) (Run 会做最终检查;RunTask 不重复检查)
//   - Config.MainCaller 必须已设置
func (h *Harness) RunTask(ctx context.Context, task Task) (*TaskResult, error) {
	// 安装 per-task 的反思收集器。如果调用方未启用 WithMemoryTools,
	// 这个 recorder 也会被安装,但因为 task_reflect 工具未注册,LLM 永远
	// 不会写入它 —— 最终 snapshot() 返回 nil,Reflections 字段不出现在归档里。
	// 这是 graceful degradation:WithMemoryTools 完全是 opt-in 的。
	rec := h.installReflection()
	defer h.clearReflection()

	// 闭包捕获 task 和 rec:在归档前把 LLM 的内省总结合并进 result,
	// 让"框架外部视角"和"LLM 内部视角"形成完整的 L4 记录。
	archive := func(outcome string, result *TaskResult) {
		result.Reflections = rec.snapshot()
		h.archiveTask(task, result, outcome)
	}
	return runTaskLoop(ctx, h.Run, archive, h.logEvent, task)
}

// runTaskLoop 是 RunTask 的纯逻辑骨架,把对 Harness 的依赖收窄为三个回调:
//
//	runRound  —— 跑一次 LLM 编辑 (生产环境是 h.Run,测试里可注入桩)
//	archive   —— 写一条 L4 归档 (生产环境是 h.archiveTask 的闭包,测试里可注入桩)
//	logEvent  —— 观测日志 (可为 nil 等价 no-op)
//
// 这种切分让"验证驱动循环"的算法可被单测覆盖,无需拉起 gateway / store / LLM。
func runTaskLoop(
	ctx context.Context,
	runRound func(ctx context.Context, userPrompt string) (*RunReport, error),
	archive func(outcome string, result *TaskResult),
	logEvent func(event string, fields map[string]any),
	task Task,
) (*TaskResult, error) {
	if strings.TrimSpace(task.Description) == "" {
		return nil, errors.New("harness: RunTask requires a non-empty Description")
	}
	if runRound == nil {
		return nil, errors.New("harness: RunTask requires a runRound function")
	}
	if archive == nil {
		archive = func(string, *TaskResult) {} // no-op
	}
	if logEvent == nil {
		logEvent = func(string, map[string]any) {}
	}

	maxRounds := task.MaxRounds
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}

	result := &TaskResult{}

	// round 0: 初始验收。如果已经通过,根本不用 LLM 介入 ——
	// 这是把"任务幂等"做到极致的关键:重跑同一个 Task 几乎零成本。
	if task.Verify != nil {
		vr, vErr := safeVerify(ctx, task.Verify)
		if vErr != nil {
			result.LastDiag = fmt.Sprintf("initial verify error: %v", vErr)
			archive("initial-verify-error", result)
			return result, vErr
		}
		result.Metrics = vr.Metrics
		result.LastDiag = vr.Diag
		if isVerifyPassed(vr) {
			result.Passed = true
			archive("noop-initial-passed", result)
			return result, nil
		}
	}

	// 演进式 prompt 的反馈历史。每轮失败追加一条结构化摘要;
	// 不存原始 Diag 全文,避免长任务的 prompt 爆炸。
	var history []string

	for round := 1; round <= maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			result.LastDiag = fmt.Sprintf("context cancelled at round %d: %v", round, err)
			archive("ctx-cancelled", result)
			return result, err
		}

		userPrompt := buildTaskPrompt(task.Description, result.LastDiag, history, round)
		logEvent("task_round_start", map[string]any{
			"round":      round,
			"max_rounds": maxRounds,
			"tag":        task.ArchiveTag,
		})

		rr, runErr := runRound(ctx, userPrompt)
		result.Rounds = round
		if rr != nil {
			result.RunReports = append(result.RunReports, rr)
		}
		if runErr != nil {
			result.LastDiag = fmt.Sprintf("LLM call failed at round %d: %v", round, runErr)
			archive("llm-error", result)
			return result, runErr
		}

		if task.BetweenRounds != nil {
			if hookErr := task.BetweenRounds(ctx, round); hookErr != nil {
				result.LastDiag = fmt.Sprintf("BetweenRounds hook failed at round %d: %v", round, hookErr)
				archive("hook-error", result)
				return result, hookErr
			}
		}

		// 没有外部 Verify 时,信任 Run 的内部判断:工具全 OK 即视为完成。
		if task.Verify == nil {
			result.Passed = rr != nil && rr.Success
			archive("no-verify", result)
			return result, nil
		}

		vr, vErr := safeVerify(ctx, task.Verify)
		if vErr != nil {
			result.LastDiag = fmt.Sprintf("verify error at round %d: %v", round, vErr)
			archive("verify-error", result)
			return result, vErr
		}
		result.Metrics = vr.Metrics
		result.LastDiag = vr.Diag
		history = append(history, summarizeVerifyForHistory(round, vr))

		if isVerifyPassed(vr) {
			result.Passed = true
			archive("verify-passed", result)
			return result, nil
		}

		logEvent("task_round_failed", map[string]any{
			"round":   round,
			"metrics": metricStatusSummary(vr.Metrics),
		})
	}

	// 轮数耗尽。归档为"未通过",但不报错 —— RunTask 完成了它的契约。
	result.Passed = false
	archive("max-rounds-exhausted", result)
	return result, nil
}

// safeVerify 把 Verify 的 panic 转成 error,避免一次回调把整个 RunTask 拖垮。
// 这非常重要:Verify 是调用方的代码,我们对它的健壮性零控制。
func safeVerify(ctx context.Context, v VerifyFunc) (vr VerifyResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("verify panicked: %v", r)
		}
	}()
	return v(ctx)
}

// isVerifyPassed 封装"什么算完成"的逻辑:
//   - 有 Metrics → 必须全部 PASS (PARTIAL/FAIL/INFRA_FAIL 都不算通过)
//   - 无 Metrics → 看 Passed 字段
//
// 故意没有"大部分 PASS 就放行"的模糊语义 —— 半通过的任务对 L4 召回是污染。
func isVerifyPassed(vr VerifyResult) bool {
	if len(vr.Metrics) > 0 {
		for _, m := range vr.Metrics {
			if m.Status != VerifyPass {
				return false
			}
		}
		return true
	}
	return vr.Passed
}

// buildTaskPrompt 把任务描述、初始反馈、历轮摘要拼成一轮的 user prompt。
//
// 设计取舍:
//   - round 1:只附"初始验收"反馈 (如果有),避免空 history 段污染 prompt
//   - round >= 2:把所有历轮以 <previous_rounds> 包起来,LLM 能看到完整轨迹
//   - 不放原始 Diag 全文进 history:由 summarizeVerifyForHistory 压成结构化块,
//     长任务也不会让 prompt 失控
func buildTaskPrompt(description, lastDiag string, history []string, round int) string {
	var sb strings.Builder
	sb.WriteString(description)

	if round == 1 && lastDiag != "" {
		sb.WriteString("\n\n<initial_verification>\n")
		sb.WriteString(strings.TrimSpace(lastDiag))
		sb.WriteString("\n</initial_verification>")
	}

	if round > 1 && len(history) > 0 {
		sb.WriteString("\n\n<previous_rounds>\n")
		for _, h := range history {
			sb.WriteString(h)
			sb.WriteByte('\n')
		}
		sb.WriteString("</previous_rounds>")
	}

	return sb.String()
}

// summarizeVerifyForHistory 把一次失败的 Verify 压缩成 LLM 友好的结构化块。
//
// 输出形如:
//
//	<round n="2">
//	metrics:
//	  - hint-penalty: FAIL (score should be 70, got 100)
//	  - semantic-error: PASS
//	diag:
//	实际计分公式没有考虑 hint penalty
//	</round>
func summarizeVerifyForHistory(round int, vr VerifyResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<round n=\"%d\">\n", round)
	if len(vr.Metrics) > 0 {
		sb.WriteString("metrics:\n")
		for _, m := range vr.Metrics {
			fmt.Fprintf(&sb, "  - %s: %s", m.Name, m.Status)
			if m.Detail != "" {
				fmt.Fprintf(&sb, " (%s)", m.Detail)
			}
			sb.WriteByte('\n')
		}
	}
	if vr.Diag != "" {
		sb.WriteString("diag:\n")
		sb.WriteString(strings.TrimSpace(vr.Diag))
		sb.WriteByte('\n')
	}
	sb.WriteString("</round>")
	return sb.String()
}

// metricStatusSummary 把 metrics 按 status 桶计数,便于 logEvent。
func metricStatusSummary(metrics []MetricResult) map[string]int {
	out := map[string]int{}
	for _, m := range metrics {
		out[string(m.Status)]++
	}
	return out
}

// archiveTask 在任务收尾时把整个 outcome 写入 L4。best-effort。
//
// 不论 Passed 与否、不论是否抛错,都尝试写一条 —— 失败案例对未来召回同样重要,
// 甚至更重要 (LLM 应该知道"上次怎么栽的")。
//
// 实现细节:
//   - 通过 type assertion 检查 Memory.L4 是否是 SessionRecordsLayer (标准布局是)
//   - 不是的话,静默跳过 (允许调用方完全自定义 L4 实现)
//   - Append 失败时记日志但不向调用方报错
//   - 成功时把 ArchiveID 回填到 result,便于调用方追踪
func (h *Harness) archiveTask(task Task, result *TaskResult, outcome string) {
	if h.memory == nil || h.memory.L4 == nil {
		return
	}
	layer, ok := h.memory.L4.(*memory.SessionRecordsLayer)
	if !ok {
		h.logEvent("task_archive_skipped", map[string]any{
			"reason": "L4 layer is not *memory.SessionRecordsLayer",
		})
		return
	}

	now := time.Now()
	id := buildArchiveID(task.ArchiveTag, now)
	summary := buildArchiveSummary(task, result, outcome)

	rec := &memory.SessionRecord{
		ID:        id,
		Timestamp: now.Unix(),
		Summary:   summary,
	}
	if err := layer.Append(rec); err != nil {
		h.logEvent("task_archive_error", map[string]any{
			"id":    id,
			"error": err.Error(),
		})
		return
	}
	result.ArchiveID = id
	h.logEvent("task_archived", map[string]any{
		"id":      id,
		"outcome": outcome,
		"passed":  result.Passed,
		"rounds":  result.Rounds,
	})
}

// buildArchiveID 给一次归档生成稳定可读的 ID。
// 形如 "fix-scoring-20251023-150405",方便人和 grep 都能用。
func buildArchiveID(tag string, now time.Time) string {
	ts := now.Format("20060102-150405")
	if tag == "" {
		return "task-" + ts
	}
	return tag + "-" + ts
}

// buildArchiveSummary 把任务摘要序列化为 JSON 字符串,塞进 SessionRecord.Summary。
//
// 用 JSON 而不是自由文本的原因:
//   - L4 是未来召回的语料。结构化字段让未来的廉价 LLM 能可靠提取
//     (例如:"召回所有 outcome=verify-passed 且 metrics 包含 hint-penalty 的会话")
//   - 同时也是人可读的 (MarshalIndent),直接 cat sessions/*.json 就能审阅
//   - 失败案例 (包括崩溃) 共享同一字段集,便于跨案例统计
//
// 后续若引入 L2 元认知层,这里的 metrics 字段就是元认知模型的主要训练样本。
func buildArchiveSummary(task Task, result *TaskResult, outcome string) string {
	payload := map[string]any{
		"outcome":     outcome,
		"passed":      result.Passed,
		"rounds":      result.Rounds,
		"description": task.Description,
		"last_diag":   result.LastDiag,
	}
	if len(result.Metrics) > 0 {
		payload["metrics"] = result.Metrics
	}
	if task.ArchiveTag != "" {
		payload["tag"] = task.ArchiveTag
	}
	// LLM 通过 task_reflect 提交的内省总结。仅当启用了 WithMemoryTools 且
	// LLM 真的调用了该工具时存在 —— 在归档里它是"为什么这次成功/失败"的
	// 内部解释,与外层的 metrics / last_diag (外部观察) 互补。
	if len(result.Reflections) > 0 {
		payload["reflections"] = result.Reflections
	}

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		// 极少发生 (我们的 payload 都是基础类型),但兜底
		return fmt.Sprintf("(archive serialize failed: %v)\n%s", err, task.Description)
	}
	return string(b)
}
