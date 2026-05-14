// memory_tools_test.go — task_reflect 工具与 reflection recorder 的单元测试。
//
// 覆盖维度:
//
//   1. reflectionRecorder 本身:add / snapshot 的语义、空字符串过滤、
//      并发安全 (并发 add 后 snapshot 数量正确)。
//
//   2. WithMemoryTools 的 API 契约:
//      - bundle 未构造时,返回 baseTools 原样 (软失败)
//      - bundle 已构造时,注册 task_reflect 工具到 h.bundle.Tools
//      - 重复调用幂等 (不重复注册)
//
//   3. installReflection / clearReflection / currentReflection 的生命周期:
//      - install 后 current 返回同一个 recorder
//      - clear 后 current 返回 nil
//      - 不同 Harness 实例彼此独立 (sync.Map 按 *Harness 隔离)
//
//   4. End-to-end 通过 runTaskLoop:在 runner 桩里手动模拟 LLM 调用
//      task_reflect (即手动 h.currentReflection().add),验证 reflections
//      最终出现在归档 JSON 里。
//
// 测试不依赖真实 LLM,但完整覆盖了从工具注册 → handler 闭包 → recorder
// 状态 → 归档落盘的整条链路。
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

	"github.com/doptime/dopharness/edit"
	"github.com/doptime/dopharness/memory"
	"github.com/doptime/dopharness/tools"
)

// ============================================================
// reflectionRecorder 单元测试
// ============================================================

func TestReflectionRecorder_AddAndSnapshot(t *testing.T) {
	r := newReflectionRecorder()
	r.add("first")
	r.add("second")

	got := r.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 insights, got %d", len(got))
	}
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("order wrong: %v", got)
	}
}

func TestReflectionRecorder_EmptySnapshotIsNil(t *testing.T) {
	r := newReflectionRecorder()
	if r.snapshot() != nil {
		t.Errorf("empty recorder should snapshot to nil for clean JSON omission")
	}
}

func TestReflectionRecorder_TrimsWhitespace_DropsEmpty(t *testing.T) {
	r := newReflectionRecorder()
	r.add("  real insight  ")
	r.add("")
	r.add("   ") // 纯空白
	r.add("\nanother\n")

	got := r.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 (after empty-skipping), got %d: %v", len(got), got)
	}
	if got[0] != "real insight" {
		t.Errorf("first insight should be trimmed: %q", got[0])
	}
	if got[1] != "another" {
		t.Errorf("second insight should be trimmed: %q", got[1])
	}
}

func TestReflectionRecorder_SnapshotIsIndependentCopy(t *testing.T) {
	r := newReflectionRecorder()
	r.add("a")

	snap := r.snapshot()
	r.add("b")
	snap2 := r.snapshot()

	if len(snap) != 1 {
		t.Errorf("first snapshot should be independent, got %d items", len(snap))
	}
	if len(snap2) != 2 {
		t.Errorf("second snapshot should reflect new add, got %d items", len(snap2))
	}

	// 修改 snap 不应影响内部状态
	snap[0] = "tampered"
	snap3 := r.snapshot()
	if snap3[0] != "a" {
		t.Errorf("internal state should be immune to external mutation: %v", snap3)
	}
}

func TestReflectionRecorder_ConcurrentAddSafe(t *testing.T) {
	r := newReflectionRecorder()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.add(fmt.Sprintf("insight-%d", i))
		}(i)
	}
	wg.Wait()

	got := r.snapshot()
	if len(got) != n {
		t.Errorf("want %d insights after concurrent adds, got %d", n, len(got))
	}
}

// ============================================================
// install / clear / current 生命周期
// ============================================================

func TestReflectionLifecycle_InstallThenClear(t *testing.T) {
	h := &Harness{}

	// 初始无 recorder
	if got := h.currentReflection(); got != nil {
		t.Fatalf("want nil before install, got %v", got)
	}

	rec := h.installReflection()
	if rec == nil {
		t.Fatal("installReflection returned nil")
	}
	if got := h.currentReflection(); got != rec {
		t.Errorf("current should match installed: %v vs %v", got, rec)
	}

	h.clearReflection()
	if got := h.currentReflection(); got != nil {
		t.Errorf("want nil after clear, got %v", got)
	}
}

func TestReflectionLifecycle_DifferentHarnessesAreIsolated(t *testing.T) {
	h1 := &Harness{}
	h2 := &Harness{}

	r1 := h1.installReflection()
	r2 := h2.installReflection()

	if r1 == r2 {
		t.Fatal("each Harness should get its own recorder")
	}
	if h1.currentReflection() != r1 {
		t.Error("h1 lookup wrong")
	}
	if h2.currentReflection() != r2 {
		t.Error("h2 lookup wrong")
	}

	// 清理一个不应影响另一个
	h1.clearReflection()
	if h1.currentReflection() != nil {
		t.Error("h1 should be cleared")
	}
	if h2.currentReflection() != r2 {
		t.Error("h2 should still be active after h1 clear")
	}
	h2.clearReflection()
}

func TestReflectionLifecycle_ClearBeforeInstall_NoPanic(t *testing.T) {
	h := &Harness{}
	// 调用 clear 而没 install 过 —— 不应 panic (Delete 幂等)
	h.clearReflection()
	if got := h.currentReflection(); got != nil {
		t.Errorf("want nil after clear of nothing, got %v", got)
	}
}

// ============================================================
// WithMemoryTools 的 API 契约
// ============================================================

// capturingBuilder 是一个最小 ToolBuilder,把每次 Build 调用记录下来用于断言。
// (与 harness_test.go 里的 stateless fakeBuilder 区分开 —— 我们这里需要观察
// 注册了哪些工具、什么签名,所以必须捕获参数。)
type capturingBuilder struct {
	mu    sync.Mutex
	built []fakeBuiltTool
}

type fakeBuiltTool struct {
	name        string
	description string
	handler     any
}

func (b *capturingBuilder) Build(name, description string, handler any) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := fakeBuiltTool{name: name, description: description, handler: handler}
	b.built = append(b.built, t)
	return t
}

// newHarnessForMemoryToolsTest 构造一个最小可用的 Harness,
// 已经"假装"调用过 AsLLMTools (即 bundle 已构造但可以为空)。
//
// 不拉起真实的 store / gateway,只为测试 memory tools 相关的逻辑。
func newHarnessForMemoryToolsTest(t *testing.T) *Harness {
	t.Helper()
	h := &Harness{
		bundle: &tools.Bundle{
			Tools:     []any{}, // 空 bundle,表示 AsLLMTools 已被调用但没添加 edit tools
			Collector: tools.NewCollector(),
		},
	}
	// 配合 archiveTask 测试时使用的真实 memory
	dir := t.TempDir()
	h.memory = memory.BuildStandardMemory(memory.LayoutDirs{Root: dir})
	return h
}

func TestWithMemoryTools_NilBundle_ReturnsBaseToolsUnchanged(t *testing.T) {
	h := &Harness{} // bundle 是 nil
	builder := &capturingBuilder{}
	base := []any{"existing-tool"}

	got := h.WithMemoryTools(builder, base)

	if len(got) != 1 || got[0] != "existing-tool" {
		t.Errorf("want base unchanged, got %v", got)
	}
	if len(builder.built) != 0 {
		t.Errorf("builder should not be invoked: %v", builder.built)
	}
}

func TestWithMemoryTools_RegistersReflectTool(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}

	got := h.WithMemoryTools(builder, h.bundle.Tools)

	if len(builder.built) != 1 {
		t.Fatalf("want 1 tool built, got %d", len(builder.built))
	}
	if builder.built[0].name != "task_reflect" {
		t.Errorf("want tool 'task_reflect', got %q", builder.built[0].name)
	}
	if !strings.Contains(builder.built[0].description, "内省") &&
		!strings.Contains(builder.built[0].description, "reflect") {
		t.Errorf("description should hint at reflection purpose, got %q",
			builder.built[0].description)
	}
	if len(got) != 1 {
		t.Errorf("returned tools should include the new tool, got %d items", len(got))
	}
	if len(h.bundle.Tools) != 1 {
		t.Errorf("bundle.Tools should be mutated; got %d", len(h.bundle.Tools))
	}
}

func TestWithMemoryTools_Idempotent(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}

	h.WithMemoryTools(builder, h.bundle.Tools)
	h.WithMemoryTools(builder, h.bundle.Tools)
	h.WithMemoryTools(builder, h.bundle.Tools)

	if len(builder.built) != 1 {
		t.Errorf("want 1 tool registration despite 3 calls, got %d", len(builder.built))
	}
	if len(h.bundle.Tools) != 1 {
		t.Errorf("bundle should have 1 tool, got %d", len(h.bundle.Tools))
	}
}

// TestWithMemoryTools_HandlerWritesToCurrentReflection 验证 handler 闭包
// 能在被调用时找到当前 active recorder 并写入。
func TestWithMemoryTools_HandlerWritesToCurrentReflection(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}
	h.WithMemoryTools(builder, h.bundle.Tools)

	// 安装一个 recorder (模拟 RunTask 在 task 开始时做的事)
	rec := h.installReflection()
	defer h.clearReflection()

	// 模拟 LLM 调用 task_reflect:从 capturingBuilder 取出 handler 直接调
	handler, ok := builder.built[0].handler.(func(*TaskReflectPayload))
	if !ok {
		t.Fatalf("handler has unexpected signature: %T", builder.built[0].handler)
	}
	handler(&TaskReflectPayload{Insight: "learned: edge case in scoring"})
	handler(&TaskReflectPayload{Insight: "pattern: use intl.NumberFormat"})

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 reflections, got %d", len(got))
	}
	if got[0] != "learned: edge case in scoring" {
		t.Errorf("first insight wrong: %q", got[0])
	}
	if got[1] != "pattern: use intl.NumberFormat" {
		t.Errorf("second insight wrong: %q", got[1])
	}
}

// TestWithMemoryTools_HandlerSilentlyDropsWhenNoRecorder 覆盖
// "工具被注册但 RunTask 外被调用"的情形 —— 不应 panic,只是丢弃。
func TestWithMemoryTools_HandlerSilentlyDropsWhenNoRecorder(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}
	h.WithMemoryTools(builder, h.bundle.Tools)

	handler := builder.built[0].handler.(func(*TaskReflectPayload))

	// 没有 install,直接调:应当无害地丢弃
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("handler panicked when no recorder active: %v", r)
		}
	}()
	handler(&TaskReflectPayload{Insight: "this should be discarded"})
	// 没 panic 即通过
}

// ============================================================
// End-to-end:reflections 出现在归档 JSON 里
//
// 我们在 runner 桩里手动模拟 LLM 调用 task_reflect (通过 h.currentReflection),
// 然后在 archive 桩里捕获 result,验证 Reflections 字段被正确填入,
// 并通过 buildArchiveSummary 序列化为 JSON 后包含 reflections 键。
// ============================================================

func TestRunTask_ReflectionsAppearInArchive(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	// 真实地注册工具,这样 RunTask 期间 currentReflection 会被同一 h 看到
	builder := &capturingBuilder{}
	h.WithMemoryTools(builder, h.bundle.Tools)

	// runner 桩模拟 LLM:每轮调用一次 task_reflect (通过查 current recorder)
	runner := func(_ context.Context, _ string) (*RunReport, error) {
		if rec := h.currentReflection(); rec != nil {
			rec.add("round insight: scoring was off-by-one")
		}
		return &RunReport{Success: true}, nil
	}

	// archive 桩捕获最终 result 供断言
	var captured *TaskResult
	archive := func(_ string, result *TaskResult) {
		cp := *result
		captured = &cp
	}

	// 模拟 RunTask 的 install / defer clear (因为我们直接调 runTaskLoop)
	rec := h.installReflection()
	defer h.clearReflection()

	// 包一层 archive 让它也把 reflections 拷进 result,模拟 RunTask 里的真闭包
	archiveWithReflections := func(outcome string, result *TaskResult) {
		result.Reflections = rec.snapshot()
		archive(outcome, result)
	}

	task := Task{
		Description: "fix scoring bug",
		Verify: func(_ context.Context) (VerifyResult, error) {
			return VerifyResult{Passed: true}, nil // 第二次 verify (round 1 后) 通过
		},
		MaxRounds: 3,
	}
	// 先让初始 verify 失败,逼着进入 round 1
	failOnce := struct {
		called bool
	}{}
	task.Verify = func(_ context.Context) (VerifyResult, error) {
		if !failOnce.called {
			failOnce.called = true
			return VerifyResult{Passed: false, Diag: "fails initially"}, nil
		}
		return VerifyResult{Passed: true}, nil
	}

	result, err := runTaskLoop(context.Background(), runner, archiveWithReflections, noopLog, task)
	if err != nil {
		t.Fatalf("loop returned err: %v", err)
	}
	if !result.Passed {
		t.Fatalf("want Passed=true")
	}
	if captured == nil {
		t.Fatal("archive was never called")
	}
	if len(captured.Reflections) != 1 {
		t.Fatalf("want 1 reflection captured, got %d: %v",
			len(captured.Reflections), captured.Reflections)
	}
	if !strings.Contains(captured.Reflections[0], "scoring was off-by-one") {
		t.Errorf("reflection content wrong: %q", captured.Reflections[0])
	}

	// 进一步验证:buildArchiveSummary 把 reflections 序列化进 JSON
	summary := buildArchiveSummary(task, captured, "verify-passed")
	var probe map[string]any
	if err := json.Unmarshal([]byte(summary), &probe); err != nil {
		t.Fatalf("summary should be valid JSON: %v\n%s", err, summary)
	}
	refs, ok := probe["reflections"].([]any)
	if !ok {
		t.Fatalf("reflections field missing or wrong type in:\n%s", summary)
	}
	if len(refs) != 1 {
		t.Errorf("want 1 reflection in JSON, got %d", len(refs))
	}
}

// TestRunTask_NoReflectionWhenMemoryToolsDisabled 是 graceful-degradation
// 的关键回归测试:如果调用方不调用 WithMemoryTools,RunTask 应当完全
// 等价于上一刀,reflections 字段不会出现在归档 JSON 里。
func TestRunTask_NoReflectionWhenMemoryToolsDisabled(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	// 注意:故意不调 WithMemoryTools

	runner := func(_ context.Context, _ string) (*RunReport, error) {
		return &RunReport{Success: true}, nil
	}

	rec := h.installReflection()
	defer h.clearReflection()

	var captured *TaskResult
	archive := func(_ string, result *TaskResult) {
		result.Reflections = rec.snapshot()
		cp := *result
		captured = &cp
	}

	task := Task{
		Description: "x",
		Verify: func(_ context.Context) (VerifyResult, error) {
			return VerifyResult{Passed: true}, nil
		},
	}
	_, err := runTaskLoop(context.Background(), runner, archive, noopLog, task)
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("archive never called")
	}
	if captured.Reflections != nil {
		t.Errorf("Reflections should be nil when LLM never called task_reflect: %v",
			captured.Reflections)
	}

	// buildArchiveSummary 应当跳过 reflections 字段
	summary := buildArchiveSummary(task, captured, "noop-initial-passed")
	if strings.Contains(summary, "reflections") {
		t.Errorf("summary should not contain 'reflections' key when nil:\n%s", summary)
	}
}

// TestRunTask_ReflectionsPersistedToDiskInL4 是完整 end-to-end 验证:
// 走 archiveTask (真实落盘),再从磁盘读取归档,确认 reflections 字段
// 在最终的 SessionRecord.Summary JSON 里。
func TestRunTask_ReflectionsPersistedToDiskInL4(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}
	h.WithMemoryTools(builder, h.bundle.Tools)

	// 安装 recorder 并模拟 LLM 调用 task_reflect
	rec := h.installReflection()
	defer h.clearReflection()
	rec.add("inside-out insight: the audit had a typo")

	task := Task{Description: "fix audit typo", ArchiveTag: "audit"}
	result := &TaskResult{
		Passed:      true,
		Rounds:      1,
		LastDiag:    "ok",
		Reflections: rec.snapshot(), // 模拟 RunTask 的 archive 闭包做的事
	}
	h.archiveTask(task, result, "verify-passed")

	if result.ArchiveID == "" {
		t.Fatal("archive should have written and set ID")
	}

	// 找到落盘文件
	files, err := os.ReadDir(filepath.Join(h.memory.L4.(*memory.SessionRecordsLayer).Dir))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 session file, got %d", len(files))
	}
	data, _ := os.ReadFile(filepath.Join(h.memory.L4.(*memory.SessionRecordsLayer).Dir, files[0].Name()))

	var rec2 memory.SessionRecord
	if err := json.Unmarshal(data, &rec2); err != nil {
		t.Fatal(err)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(rec2.Summary), &inner); err != nil {
		t.Fatalf("inner summary not JSON: %v\n%s", err, rec2.Summary)
	}
	refs, ok := inner["reflections"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("reflections missing in persisted record:\n%s", rec2.Summary)
	}
	if !strings.Contains(refs[0].(string), "had a typo") {
		t.Errorf("reflection content wrong: %v", refs[0])
	}
}

// ============================================================
// 防御性回归:确保 reflection 失败不会污染主流程
// ============================================================

// TestWithMemoryTools_HandlerErrorDoesNotPropagate
// 表面看上去 handler 没有错误返回 (因为 doptime/llm 的 ToolBuilder 约定就是 func(*T)),
// 但万一未来改造 handler 签名,我们也希望 reflection 错误不影响 main task。
// 此处验证当前签名,作为对未来重构的契约保护。
func TestWithMemoryTools_HandlerSignatureIsErrorFree(t *testing.T) {
	h := newHarnessForMemoryToolsTest(t)
	builder := &capturingBuilder{}
	h.WithMemoryTools(builder, h.bundle.Tools)

	handler := builder.built[0].handler
	if _, ok := handler.(func(*TaskReflectPayload)); !ok {
		t.Errorf("handler signature changed from func(*TaskReflectPayload) — "+
			"if this is intentional, update RunTask to handle errors gracefully. got: %T",
			handler)
	}
}

// 编译期断言:reflectionRecorder 必须实现 add/snapshot,以便未来改名时立刻失败。
var _ interface {
	add(string)
	snapshot() []string
} = (*reflectionRecorder)(nil)

// 占位:导入但暂未使用的 errors/edit 包(给将来更深入的集成测试预留)
var (
	_ = errors.New
	_ = edit.OutcomeApplied
)
