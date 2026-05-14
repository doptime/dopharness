// task_test.go — RunTask / runTaskLoop 的单元测试。
//
// 测试策略:
//
//	runTaskLoop 把 RunTask 的"验证驱动循环"算法从 Harness 状态里抽出来,
//	接受三个回调 (runRound, archive, logEvent)。这让我们可以用纯函数桩
//	完整覆盖循环的所有路径,不需要拉起 gateway / store / LLM。
//
//	对真正绑在 Harness 上的部分 (archiveTask 写盘) —— 我们构造一个最小
//	Harness (只填 memory 字段) 直接测,覆盖 L4 落盘和 type-assertion 跳过。
//
// 覆盖的场景:
//   - 初始 Verify 已通过 → 零 LLM 调用
//   - 初始 Verify 出错 → 报错并归档
//   - Verify 多轮失败后通过 → 归档为 verify-passed
//   - Verify 始终失败 → 归档为 max-rounds-exhausted (err == nil)
//   - 无 Verify → 信任 Run.Success
//   - Metrics 全 PASS → 通过 (即使 Passed=false 也忽略)
//   - Metrics 有 PARTIAL → 不通过
//   - Run 自身报错 → 归档为 llm-error 并返回 err
//   - BetweenRounds 报错 → 归档为 hook-error 并返回 err
//   - Verify panic → 被 recover,归档为 verify-error
//   - ctx 取消 → 归档为 ctx-cancelled
//   - 空 Description → 立即报错
//   - buildTaskPrompt 在不同 round / lastDiag / history 组合下的输出
//   - archiveTask 写入真实磁盘 + L4 类型不匹配的优雅跳过

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/doptime/dopharness/memory"
)

// ============================================================
// 测试辅助
// ============================================================

// noopLog 是丢弃所有事件的日志桩。
func noopLog(string, map[string]any) {}

// runnerStub 是 runRound 的可观测桩。
//
// 用法:
//
//	stub := newRunnerStub().
//	    returning(&RunReport{Success: true}, nil).
//	    returning(&RunReport{Success: false}, nil)
//	stub.calls 在循环结束后包含每一轮的 prompt。
type runnerStub struct {
	mu      sync.Mutex
	calls   []string         // 收到的每一轮 prompt
	reports []*RunReport     // 每一轮要返回的 report
	errs    []error          // 每一轮要返回的 err (并行于 reports)
	pos     int              // 已消耗到第几条
}

func newRunnerStub() *runnerStub { return &runnerStub{} }

// returning 追加一组下一次调用的返回值。
func (s *runnerStub) returning(rep *RunReport, err error) *runnerStub {
	s.reports = append(s.reports, rep)
	s.errs = append(s.errs, err)
	return s
}

// run 是塞进 runTaskLoop 的真正回调。
func (s *runnerStub) run(_ context.Context, prompt string) (*RunReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, prompt)
	if s.pos >= len(s.reports) {
		// 默认成功,避免测试因桩耗尽而崩
		return &RunReport{Success: true}, nil
	}
	r, e := s.reports[s.pos], s.errs[s.pos]
	s.pos++
	return r, e
}

// archiveCapture 是 archive 回调的可观测桩。
type archiveCapture struct {
	outcomes []string
	results  []TaskResult // 每次调用时 *result 的快照
}

func (a *archiveCapture) fn(outcome string, result *TaskResult) {
	a.outcomes = append(a.outcomes, outcome)
	a.results = append(a.results, *result) // 值拷贝快照
}

// constVerify 返回一个固定结果的 VerifyFunc。
func constVerify(vr VerifyResult) VerifyFunc {
	return func(_ context.Context) (VerifyResult, error) { return vr, nil }
}

// failingVerify 返回固定的 Passed=false 反馈。
func failingVerify(diag string) VerifyFunc {
	return constVerify(VerifyResult{Passed: false, Diag: diag})
}

// scriptedVerify 按调用次序返回不同结果。第 i 次调用返回 results[i-1] / errs[i-1]。
// 超出范围时返回最后一项。
type scriptedVerify struct {
	mu      sync.Mutex
	pos     int
	results []VerifyResult
	errs    []error
}

func newScriptedVerify() *scriptedVerify { return &scriptedVerify{} }

func (s *scriptedVerify) returning(vr VerifyResult, err error) *scriptedVerify {
	s.results = append(s.results, vr)
	s.errs = append(s.errs, err)
	return s
}

func (s *scriptedVerify) fn(_ context.Context) (VerifyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.pos
	if i >= len(s.results) {
		i = len(s.results) - 1 // 钉住最后一项,避免越界
	}
	s.pos++
	return s.results[i], s.errs[i]
}

// ============================================================
// runTaskLoop —— 主流程测试
// ============================================================

func TestRunTaskLoop_InitialVerifyPasses_NoLLMCall(t *testing.T) {
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{
		Description: "should already be done",
		Verify:      constVerify(VerifyResult{Passed: true}),
	}

	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if !result.Passed {
		t.Errorf("want Passed=true")
	}
	if result.Rounds != 0 {
		t.Errorf("want Rounds=0, got %d", result.Rounds)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runRound should not be called, got %d call(s)", len(runner.calls))
	}
	if len(arch.outcomes) != 1 || arch.outcomes[0] != "noop-initial-passed" {
		t.Errorf("want one archive 'noop-initial-passed', got %v", arch.outcomes)
	}
}

func TestRunTaskLoop_PassesAfterTwoRounds(t *testing.T) {
	verify := newScriptedVerify().
		returning(VerifyResult{Passed: false, Diag: "first failure"}, nil).
		returning(VerifyResult{Passed: false, Diag: "second failure"}, nil).
		returning(VerifyResult{Passed: true}, nil)

	runner := newRunnerStub() // 默认全成功
	arch := &archiveCapture{}

	task := Task{
		Description: "fix it",
		Verify:      verify.fn,
		MaxRounds:   5,
	}

	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if !result.Passed {
		t.Errorf("want Passed=true")
	}
	if result.Rounds != 2 {
		t.Errorf("want Rounds=2, got %d", result.Rounds)
	}
	if len(runner.calls) != 2 {
		t.Errorf("want 2 LLM calls, got %d", len(runner.calls))
	}
	if len(result.RunReports) != 2 {
		t.Errorf("want 2 RunReports, got %d", len(result.RunReports))
	}
	if len(arch.outcomes) != 1 || arch.outcomes[0] != "verify-passed" {
		t.Errorf("want archive 'verify-passed', got %v", arch.outcomes)
	}

	// round 2 的 prompt 应该包含 <previous_rounds> 块,带 round 1 编辑*之后*的反馈。
	// 在我们的脚本里,初始 verify 返回 "first failure" (round 0,只塞进 round 1
	// 的 <initial_verification> 段);round 1 编辑后的 verify 返回 "second failure"
	// (这是 history 里第一条 round 摘要)。所以 round 2 的 prompt 应含 "second failure"
	// 但不含 "first failure" —— 后者作用范围只到 round 1。
	round2Prompt := runner.calls[1]
	if !strings.Contains(round2Prompt, "<previous_rounds>") {
		t.Errorf("round-2 prompt missing <previous_rounds>:\n%s", round2Prompt)
	}
	if !strings.Contains(round2Prompt, "second failure") {
		t.Errorf("round-2 prompt missing round-1 verify diag:\n%s", round2Prompt)
	}
	if strings.Contains(round2Prompt, "first failure") {
		t.Errorf("round-2 prompt should NOT contain initial verify diag (only round 1 should):\n%s", round2Prompt)
	}
	if strings.Contains(round2Prompt, "<initial_verification>") {
		t.Errorf("round-2 prompt should NOT have <initial_verification> tag:\n%s", round2Prompt)
	}
	// 同时,round 1 的 prompt 应该有 <initial_verification> 与 "first failure"
	round1Prompt := runner.calls[0]
	if !strings.Contains(round1Prompt, "<initial_verification>") {
		t.Errorf("round-1 prompt missing <initial_verification>:\n%s", round1Prompt)
	}
	if !strings.Contains(round1Prompt, "first failure") {
		t.Errorf("round-1 prompt missing initial verify diag:\n%s", round1Prompt)
	}
}

func TestRunTaskLoop_MaxRoundsExhausted_NoError(t *testing.T) {
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{
		Description: "impossible",
		Verify:      failingVerify("still broken"),
		MaxRounds:   2,
	}

	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if err != nil {
		t.Fatalf("耗尽轮数不应报错,got: %v", err)
	}
	if result.Passed {
		t.Errorf("want Passed=false")
	}
	if result.Rounds != 2 {
		t.Errorf("want Rounds=2, got %d", result.Rounds)
	}
	if len(arch.outcomes) != 1 || arch.outcomes[0] != "max-rounds-exhausted" {
		t.Errorf("want archive 'max-rounds-exhausted', got %v", arch.outcomes)
	}
	if result.LastDiag != "still broken" {
		t.Errorf("want LastDiag carried through, got %q", result.LastDiag)
	}
}

func TestRunTaskLoop_DefaultMaxRoundsWhenZero(t *testing.T) {
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{
		Description: "x",
		Verify:      failingVerify("nope"),
		// MaxRounds 留 0 → 默认 DefaultMaxRounds
	}

	result, _ := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if result.Rounds != DefaultMaxRounds {
		t.Errorf("want Rounds=%d (default), got %d", DefaultMaxRounds, result.Rounds)
	}
}

func TestRunTaskLoop_NoVerify_TrustsRunSuccess(t *testing.T) {
	runner := newRunnerStub().returning(&RunReport{Success: true}, nil)
	arch := &archiveCapture{}

	task := Task{Description: "refactor X", Verify: nil}

	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if !result.Passed {
		t.Errorf("want Passed=true (delegated to Run.Success)")
	}
	if result.Rounds != 1 {
		t.Errorf("want Rounds=1, got %d", result.Rounds)
	}
	if arch.outcomes[0] != "no-verify" {
		t.Errorf("want archive 'no-verify', got %v", arch.outcomes)
	}
}

func TestRunTaskLoop_NoVerify_RunReportsFailure(t *testing.T) {
	runner := newRunnerStub().returning(&RunReport{Success: false}, nil)
	arch := &archiveCapture{}

	task := Task{Description: "x", Verify: nil}

	result, _ := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if result.Passed {
		t.Errorf("want Passed=false when Run says Success=false")
	}
}

// ============================================================
// Metrics 的判停语义
// ============================================================

func TestRunTaskLoop_MetricsAllPass_OverridesPassed(t *testing.T) {
	verify := constVerify(VerifyResult{
		Passed: false, // 故意设错;Metrics 全 PASS 应覆盖
		Metrics: []MetricResult{
			{Name: "a", Status: VerifyPass},
			{Name: "b", Status: VerifyPass},
		},
	})
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{Description: "x", Verify: verify}
	result, _ := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if !result.Passed {
		t.Errorf("metrics all PASS should override Passed=false")
	}
	if len(runner.calls) != 0 {
		t.Errorf("initial verify passed, should not call LLM")
	}
}

func TestRunTaskLoop_MetricsOnePartial_DoesNotPass(t *testing.T) {
	verify := constVerify(VerifyResult{
		Passed: true, // 故意设对;Metrics 有 PARTIAL 应覆盖
		Metrics: []MetricResult{
			{Name: "a", Status: VerifyPass},
			{Name: "b", Status: VerifyPartial},
		},
	})
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{Description: "x", Verify: verify, MaxRounds: 1}
	result, _ := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if result.Passed {
		t.Errorf("PARTIAL in metrics should not pass overall")
	}
	if result.Rounds != 1 {
		t.Errorf("want Rounds=1, got %d", result.Rounds)
	}
}

// ============================================================
// 错误与崩溃处理
// ============================================================

func TestRunTaskLoop_RunError_ArchivesAndReturnsErr(t *testing.T) {
	llmErr := errors.New("network down")
	runner := newRunnerStub().returning(nil, llmErr)
	arch := &archiveCapture{}

	task := Task{
		Description: "x",
		Verify:      failingVerify("initial fail"),
		MaxRounds:   3,
	}
	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if !errors.Is(err, llmErr) {
		t.Errorf("want llmErr, got %v", err)
	}
	if result.Rounds != 1 {
		t.Errorf("want Rounds=1 (bailed mid-round), got %d", result.Rounds)
	}
	if len(arch.outcomes) != 1 || arch.outcomes[0] != "llm-error" {
		t.Errorf("want archive 'llm-error', got %v", arch.outcomes)
	}
	if !strings.Contains(result.LastDiag, "network down") {
		t.Errorf("LastDiag should mention error, got %q", result.LastDiag)
	}
}

func TestRunTaskLoop_BetweenRoundsError_ArchivesAndReturnsErr(t *testing.T) {
	hookErr := errors.New("hmr failed")
	runner := newRunnerStub() // default OK
	arch := &archiveCapture{}

	task := Task{
		Description: "x",
		Verify:      failingVerify("init"),
		MaxRounds:   3,
		BetweenRounds: func(_ context.Context, _ int) error {
			return hookErr
		},
	}
	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if !errors.Is(err, hookErr) {
		t.Errorf("want hookErr, got %v", err)
	}
	if arch.outcomes[0] != "hook-error" {
		t.Errorf("want archive 'hook-error', got %v", arch.outcomes)
	}
	if result.Rounds != 1 {
		t.Errorf("want Rounds=1 (bailed in hook), got %d", result.Rounds)
	}
}

func TestRunTaskLoop_BetweenRoundsCalledAfterEachEdit(t *testing.T) {
	verify := newScriptedVerify().
		returning(VerifyResult{Passed: false, Diag: "1"}, nil).
		returning(VerifyResult{Passed: false, Diag: "2"}, nil).
		returning(VerifyResult{Passed: true}, nil)

	runner := newRunnerStub()
	arch := &archiveCapture{}

	var hookCalls []int
	task := Task{
		Description: "x",
		Verify:      verify.fn,
		MaxRounds:   5,
		BetweenRounds: func(_ context.Context, round int) error {
			hookCalls = append(hookCalls, round)
			return nil
		},
	}
	result, _ := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if !result.Passed {
		t.Fatalf("expected pass on round 2 verify")
	}
	if len(hookCalls) != 2 {
		t.Errorf("want hook called twice (rounds 1 and 2), got %v", hookCalls)
	}
	if hookCalls[0] != 1 || hookCalls[1] != 2 {
		t.Errorf("want hook called with rounds [1, 2], got %v", hookCalls)
	}
}

func TestRunTaskLoop_VerifyPanicRecovered(t *testing.T) {
	verify := func(_ context.Context) (VerifyResult, error) {
		panic("boom")
	}
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{Description: "x", Verify: verify}
	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if err == nil {
		t.Fatal("want error from recovered panic, got nil")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("err should mention panic, got %v", err)
	}
	if arch.outcomes[0] != "initial-verify-error" {
		t.Errorf("want archive 'initial-verify-error', got %v", arch.outcomes)
	}
	if result == nil {
		t.Fatal("result should not be nil even on panic")
	}
}

func TestRunTaskLoop_VerifyErrorOnSubsequentRound(t *testing.T) {
	// 第一次 Verify 返回失败 → 进入第 1 轮编辑;
	// 第二次 Verify (在第 1 轮编辑之后) 报 error → 归档为 verify-error 并返回 err。
	verify := newScriptedVerify().
		returning(VerifyResult{Passed: false, Diag: "initial fail"}, nil).
		returning(VerifyResult{}, errors.New("audit backend crashed"))
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{Description: "x", Verify: verify.fn, MaxRounds: 3}
	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)

	if err == nil {
		t.Fatal("expected error from verify failure")
	}
	if arch.outcomes[0] != "verify-error" {
		t.Errorf("want archive 'verify-error', got %v", arch.outcomes)
	}
	if result.Rounds != 1 {
		t.Errorf("want Rounds=1 (got into round 1 then bailed), got %d", result.Rounds)
	}
}

func TestRunTaskLoop_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{
		Description: "x",
		Verify:      failingVerify("init"), // 不需要走到 verify
		MaxRounds:   3,
	}
	_, err := runTaskLoop(ctx, runner.run, arch.fn, noopLog, task)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	if arch.outcomes[0] != "ctx-cancelled" {
		t.Errorf("want archive 'ctx-cancelled', got %v", arch.outcomes)
	}
}

func TestRunTaskLoop_EmptyDescription(t *testing.T) {
	runner := newRunnerStub()
	arch := &archiveCapture{}

	task := Task{Description: "   "} // 空白也算空

	result, err := runTaskLoop(context.Background(), runner.run, arch.fn, noopLog, task)
	if err == nil {
		t.Fatal("want error for empty Description")
	}
	if result != nil {
		t.Errorf("want nil result on input error, got %+v", result)
	}
	if len(arch.outcomes) != 0 {
		t.Errorf("no archive should happen on input error, got %v", arch.outcomes)
	}
}

// ============================================================
// 纯函数辅助测试
// ============================================================

func TestBuildTaskPrompt_Round1NoDiag(t *testing.T) {
	out := buildTaskPrompt("do X", "", nil, 1)
	if out != "do X" {
		t.Errorf("want exactly 'do X', got %q", out)
	}
}

func TestBuildTaskPrompt_Round1WithDiag(t *testing.T) {
	out := buildTaskPrompt("do X", "initial verify says scoring is off", nil, 1)
	if !strings.Contains(out, "do X") {
		t.Error("description missing")
	}
	if !strings.Contains(out, "<initial_verification>") {
		t.Error("initial_verification tag missing")
	}
	if !strings.Contains(out, "scoring is off") {
		t.Error("diag content missing")
	}
	if strings.Contains(out, "<previous_rounds>") {
		t.Error("round 1 should not have previous_rounds")
	}
}

func TestBuildTaskPrompt_Round2WithHistory(t *testing.T) {
	hist := []string{`<round n="1">stuff from round 1</round>`}
	out := buildTaskPrompt("do X", "current diag", hist, 2)
	if !strings.Contains(out, "<previous_rounds>") {
		t.Error("previous_rounds wrapper missing")
	}
	if !strings.Contains(out, `<round n="1">`) {
		t.Error("history entry missing")
	}
	if strings.Contains(out, "<initial_verification>") {
		t.Error("round 2 should not emit initial_verification")
	}
}

func TestIsVerifyPassed(t *testing.T) {
	cases := []struct {
		name string
		vr   VerifyResult
		want bool
	}{
		{"no metrics, passed=true", VerifyResult{Passed: true}, true},
		{"no metrics, passed=false", VerifyResult{Passed: false}, false},
		{"metrics all pass overrides passed=false", VerifyResult{
			Passed: false,
			Metrics: []MetricResult{
				{Status: VerifyPass}, {Status: VerifyPass},
			},
		}, true},
		{"metrics one fail overrides passed=true", VerifyResult{
			Passed: true,
			Metrics: []MetricResult{
				{Status: VerifyPass}, {Status: VerifyFail},
			},
		}, false},
		{"partial counts as not pass", VerifyResult{
			Metrics: []MetricResult{{Status: VerifyPartial}},
		}, false},
		{"infra fail counts as not pass", VerifyResult{
			Metrics: []MetricResult{{Status: VerifyInfra}},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isVerifyPassed(c.vr); got != c.want {
				t.Errorf("isVerifyPassed: want %v, got %v", c.want, got)
			}
		})
	}
}

func TestSummarizeVerifyForHistory_WithMetricsAndDiag(t *testing.T) {
	out := summarizeVerifyForHistory(2, VerifyResult{
		Diag: "the scorer ignores hints",
		Metrics: []MetricResult{
			{Name: "hint-penalty", Status: VerifyFail, Detail: "want 70, got 100"},
			{Name: "happy-path", Status: VerifyPass},
		},
	})
	if !strings.Contains(out, `<round n="2">`) {
		t.Error("round wrapper missing")
	}
	if !strings.Contains(out, "hint-penalty: FAIL (want 70, got 100)") {
		t.Errorf("metric line format wrong:\n%s", out)
	}
	if !strings.Contains(out, "happy-path: PASS") {
		t.Errorf("second metric missing:\n%s", out)
	}
	if !strings.Contains(out, "the scorer ignores hints") {
		t.Error("diag missing")
	}
}

func TestSummarizeVerifyForHistory_DiagOnly(t *testing.T) {
	out := summarizeVerifyForHistory(1, VerifyResult{Diag: "just text"})
	if strings.Contains(out, "metrics:") {
		t.Errorf("metrics section should be absent when no metrics:\n%s", out)
	}
	if !strings.Contains(out, "just text") {
		t.Error("diag missing")
	}
}

func TestMetricStatusSummary(t *testing.T) {
	got := metricStatusSummary([]MetricResult{
		{Status: VerifyPass}, {Status: VerifyPass}, {Status: VerifyFail},
	})
	if got["PASS"] != 2 || got["FAIL"] != 1 {
		t.Errorf("counts wrong: %v", got)
	}
}

func TestBuildArchiveID(t *testing.T) {
	// 不直接验证时间戳具体值,但格式应稳定
	id1 := buildArchiveID("", parsedTime("2025-05-13T10:23:45Z"))
	if id1 != "task-20250513-102345" {
		t.Errorf("want 'task-20250513-102345', got %q", id1)
	}
	id2 := buildArchiveID("fix-scoring", parsedTime("2025-05-13T10:23:45Z"))
	if id2 != "fix-scoring-20250513-102345" {
		t.Errorf("want 'fix-scoring-20250513-102345', got %q", id2)
	}
}

func TestBuildArchiveSummary_IsValidJSONWithExpectedFields(t *testing.T) {
	task := Task{
		Description: "fix scoring",
		ArchiveTag:  "scoring-bug",
	}
	result := &TaskResult{
		Passed:   true,
		Rounds:   2,
		LastDiag: "all green",
		Metrics: []MetricResult{
			{Name: "a", Status: VerifyPass},
		},
	}
	s := buildArchiveSummary(task, result, "verify-passed")

	var probe map[string]any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		t.Fatalf("summary should be valid JSON: %v\n%s", err, s)
	}

	checks := map[string]any{
		"outcome":     "verify-passed",
		"passed":      true,
		"description": "fix scoring",
		"last_diag":   "all green",
		"tag":         "scoring-bug",
	}
	for k, want := range checks {
		got, ok := probe[k]
		if !ok {
			t.Errorf("missing field %q in:\n%s", k, s)
			continue
		}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("field %q: want %v, got %v", k, want, got)
		}
	}
	if _, ok := probe["metrics"]; !ok {
		t.Error("metrics field missing")
	}
	// rounds 在 JSON 反序列化后是 float64
	if r, _ := probe["rounds"].(float64); int(r) != 2 {
		t.Errorf("rounds wrong: %v", probe["rounds"])
	}
}

// ============================================================
// archiveTask —— 集成测试:真实写盘 + 类型分支
// ============================================================

func TestArchiveTask_WritesL4RecordToDisk(t *testing.T) {
	dir := t.TempDir()
	mem := memory.BuildStandardMemory(memory.LayoutDirs{Root: dir})

	h := &Harness{memory: mem}

	task := Task{Description: "fix scoring", ArchiveTag: "test-tag"}
	result := &TaskResult{Passed: true, Rounds: 2, LastDiag: "ok"}
	h.archiveTask(task, result, "verify-passed")

	if result.ArchiveID == "" {
		t.Fatal("ArchiveID should be filled in after successful archive")
	}
	if !strings.HasPrefix(result.ArchiveID, "test-tag-") {
		t.Errorf("ArchiveID should start with tag, got %q", result.ArchiveID)
	}

	// 验证磁盘上确实有这个文件
	sessionsDir := filepath.Join(dir, "sessions")
	files, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("sessions dir not created: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 session file, got %d", len(files))
	}

	// 内容应是合法 JSON 且含我们的字段
	data, _ := os.ReadFile(filepath.Join(sessionsDir, files[0].Name()))
	var rec memory.SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("session record not valid JSON: %v", err)
	}
	if rec.ID != result.ArchiveID {
		t.Errorf("on-disk ID mismatch: %q vs %q", rec.ID, result.ArchiveID)
	}
	// Summary 本身也应是 JSON
	var summary map[string]any
	if err := json.Unmarshal([]byte(rec.Summary), &summary); err != nil {
		t.Fatalf("summary not valid JSON: %v\n%s", err, rec.Summary)
	}
	if summary["outcome"] != "verify-passed" {
		t.Errorf("summary.outcome wrong: %v", summary["outcome"])
	}
	if summary["passed"] != true {
		t.Errorf("summary.passed wrong: %v", summary["passed"])
	}
}

func TestArchiveTask_NilMemory_NoOp(t *testing.T) {
	h := &Harness{} // memory nil
	task := Task{Description: "x"}
	result := &TaskResult{Passed: true}

	// 不应 panic;不应填 ArchiveID
	h.archiveTask(task, result, "some-outcome")
	if result.ArchiveID != "" {
		t.Errorf("ArchiveID should stay empty when memory is nil")
	}
}

func TestArchiveTask_NilL4Layer_NoOp(t *testing.T) {
	h := &Harness{memory: &memory.Memory{}} // 所有 layer 都 nil
	task := Task{Description: "x"}
	result := &TaskResult{Passed: true}

	h.archiveTask(task, result, "some-outcome")
	if result.ArchiveID != "" {
		t.Errorf("ArchiveID should stay empty when L4 is nil")
	}
}

// stubLayer 是一个非 SessionRecordsLayer 的 Layer 实现,用于覆盖
// "L4 类型不匹配时静默跳过"的分支。
type stubLayer struct{}

func (stubLayer) Prefix() string { return "stub" }
func (stubLayer) Render(*memory.RenderCtx) (string, string, error) {
	return "", "", nil
}

func TestArchiveTask_NonSessionRecordsLayer_SkippedGracefully(t *testing.T) {
	h := &Harness{
		memory: &memory.Memory{L4: stubLayer{}},
	}
	task := Task{Description: "x"}
	result := &TaskResult{Passed: true}

	h.archiveTask(task, result, "some-outcome")
	if result.ArchiveID != "" {
		t.Errorf("ArchiveID should stay empty when L4 is custom (non-SessionRecordsLayer)")
	}
}

// ============================================================
// 测试小工具
// ============================================================

// parsedTime 把 RFC3339 字符串解析为 time.Time;失败 panic (测试代码,可接受)。
func parsedTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
