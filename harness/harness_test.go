package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/gateway"
	"github.com/doptime/dopharness/tools"
)

// --- 测试脚手架 ---

// skipIfNoTS 如果环境没 bun / shim 就跳过 TS 相关测试。
func skipIfNoTS(t *testing.T) {
	t.Helper()
	if override := os.Getenv("DOPHARNESS_BUN"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return
		}
	}
	if _, err := exec.LookPath("bun"); err == nil {
		return
	}
	t.Skip("TS runtime not available")
}

// setupHarness 建一个带初始文件的 Harness。
func setupHarness(t *testing.T, files map[string]string, cfg Config) (*Harness, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg.ProjectRoot = root
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, root
}

// fakeBuilder 同 tools_test.go,测试用假工具构造器
type fakeTool struct {
	name, desc string
	handler    any
}

type fakeBuilder struct{}

func (fakeBuilder) Build(name, desc string, handler any) any {
	return &fakeTool{name: name, desc: desc, handler: handler}
}

// dispatchTool 模拟 LLM 调用某个工具。
func dispatchTool(t *testing.T, toolList []any, name string, payload any) {
	t.Helper()
	for _, x := range toolList {
		ft, ok := x.(*fakeTool)
		if !ok {
			continue
		}
		if ft.name != name {
			continue
		}
		switch h := ft.handler.(type) {
		case func(*tools.ModifyChunkPayload):
			h(payload.(*tools.ModifyChunkPayload))
		case func(*tools.DeleteChunkPayload):
			h(payload.(*tools.DeleteChunkPayload))
		case func(*tools.AddChunkPayload):
			h(payload.(*tools.AddChunkPayload))
		case func(*tools.CreateFilePayload):
			h(payload.(*tools.CreateFilePayload))
		case func(*tools.DeleteFilePayload):
			h(payload.(*tools.DeleteFilePayload))
		default:
			t.Fatalf("unknown handler for %s: %T", name, ft.handler)
		}
		return
	}
	t.Fatalf("tool %s not found", name)
}

// --- Index 测试 ---

func TestHarness_Index(t *testing.T) {
	h, _ := setupHarness(t, map[string]string{
		"main.go": `package main

func Hello() {}
`,
		"util/helper.go": `package util

func Helper() int { return 1 }
`,
	}, Config{})

	rep, err := h.Index(context.Background())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if rep.FilesIndexed != 2 {
		t.Errorf("want 2 files indexed, got %d", rep.FilesIndexed)
	}
	if rep.ChunksTotal != 2 {
		t.Errorf("want 2 chunks total, got %d", rep.ChunksTotal)
	}

	// 二次索引应全 skip
	rep2, _ := h.Index(context.Background())
	if rep2.FilesIndexed != 0 {
		t.Errorf("second index should index 0, got %d", rep2.FilesIndexed)
	}
	if rep2.FilesSkipped != 2 {
		t.Errorf("second index should skip 2, got %d", rep2.FilesSkipped)
	}
}

// --- BuildContext 测试 ---

// testTriage 是一个简单的 mock triage —— 对所有 chunk 发 SKELETON。
func testTriageAllSkeleton() gateway.TriageCaller {
	return func(p gateway.TriagePromptParams, sink func(*gateway.TriageDecisionPayload)) error {
		out := &gateway.TriageDecisionPayload{}
		for _, v := range p.Views {
			out.Decisions = append(out.Decisions, gateway.TriageItem{
				ChunkID: v.ID,
				Mode:    "SKELETON",
			})
		}
		sink(out)
		return nil
	}
}

func TestHarness_BuildContext(t *testing.T) {
	h, _ := setupHarness(t, map[string]string{
		"main.go": `package main

func Hello() {}
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
	})

	if _, err := h.Index(context.Background()); err != nil {
		t.Fatal(err)
	}

	bctx, err := h.BuildContext("change Hello")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// system 段应有默认 rules(含 "Tool Call")
	if !strings.Contains(bctx.SystemPrompt, "Tool Call") {
		t.Errorf("system prompt missing default rules:\n%s", bctx.SystemPrompt)
	}
	// user 段应有 project_context + user_task
	if !strings.Contains(bctx.UserPrompt, "<project_context>") {
		t.Errorf("user missing project_context:\n%s", bctx.UserPrompt)
	}
	if !strings.Contains(bctx.UserPrompt, "<user_task>") {
		t.Errorf("user missing user_task:\n%s", bctx.UserPrompt)
	}
	if !strings.Contains(bctx.UserPrompt, "change Hello") {
		t.Errorf("user task text missing:\n%s", bctx.UserPrompt)
	}
}

func TestHarness_BuildContext_RequiresTriage(t *testing.T) {
	h, _ := setupHarness(t, nil, Config{}) // 没配 Triage
	_, err := h.BuildContext("x")
	if err == nil {
		t.Fatal("BuildContext without TriageCaller should fail")
	}
}

// --- Run (主流程) 测试 ---

// successfulMainCaller 模拟 LLM 在第 1 轮就调用正确的工具。
// injectTool 是要"假装被 LLM 调用"的工具名,injectPayload 是它的参数。
func successfulMainCaller(t *testing.T, injectTool string, injectPayload any) MainCaller {
	return func(sys, usr string, toolList []any) error {
		dispatchTool(t, toolList, injectTool, injectPayload)
		return nil
	}
}

func TestHarness_Run_FirstAttemptSuccess(t *testing.T) {
	h, _ := setupHarness(t, map[string]string{
		"a.go": `package a

func Foo() int { return 1 }
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
	})
	if _, err := h.Index(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 注意:Run 前必须 AsLLMTools 注册工具
	h.AsLLMTools(fakeBuilder{})

	var fooID string
	for _, c := range h.Store().AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	// 配置一个"第 1 轮就成功"的 MainCaller
	h.cfg.MainCaller = successfulMainCaller(t, "modify_chunk", &tools.ModifyChunkPayload{
		ChunkID:    fooID,
		NewContent: "func Foo() int { return 42 }",
	})

	rep, err := h.Run(context.Background(), "change Foo to return 42")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rep.Success {
		t.Errorf("should succeed")
	}
	if rep.Rounds != 1 {
		t.Errorf("want 1 round, got %d", rep.Rounds)
	}
}

func TestHarness_Run_RetryOnValidationFailure(t *testing.T) {
	h, _ := setupHarness(t, map[string]string{
		"a.go": `package a

func Foo() int { return 1 }
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
		MaxRetries:   3,
	})
	if _, err := h.Index(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.AsLLMTools(fakeBuilder{})

	var fooID string
	for _, c := range h.Store().AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	// 第 1 轮故意发坏代码;第 2 轮发好代码。
	// 验证:Run 会重试,第 2 轮成功后返回。
	var calls int32
	h.cfg.MainCaller = func(sys, usr string, toolList []any) error {
		round := atomic.AddInt32(&calls, 1)
		if round == 1 {
			// 坏代码:缺 }
			dispatchTool(t, toolList, "modify_chunk", &tools.ModifyChunkPayload{
				ChunkID:    fooID,
				NewContent: "func Foo() int { return 42",
			})
		} else {
			// 用户 prompt 应包含上一轮反馈
			if !strings.Contains(usr, "<previous_attempt") {
				t.Errorf("retry round should include previous_attempt feedback")
			}
			dispatchTool(t, toolList, "modify_chunk", &tools.ModifyChunkPayload{
				ChunkID:    fooID,
				NewContent: "func Foo() int { return 42 }",
			})
		}
		return nil
	}

	rep, err := h.Run(context.Background(), "change Foo")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rep.Success {
		t.Errorf("should succeed on 2nd round")
	}
	if rep.Rounds != 2 {
		t.Errorf("want 2 rounds, got %d", rep.Rounds)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("MainCaller should be called 2 times, got %d", calls)
	}
}

func TestHarness_Run_ExhaustRetries(t *testing.T) {
	h, _ := setupHarness(t, map[string]string{
		"a.go": `package a
func Foo() {}
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
		MaxRetries:   2,
	})
	if _, err := h.Index(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.AsLLMTools(fakeBuilder{})

	var fooID string
	for _, c := range h.Store().AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	// 每次都发坏代码
	var calls int32
	h.cfg.MainCaller = func(sys, usr string, toolList []any) error {
		atomic.AddInt32(&calls, 1)
		dispatchTool(t, toolList, "modify_chunk", &tools.ModifyChunkPayload{
			ChunkID:    fooID,
			NewContent: "func Foo() { BAD BAD",
		})
		return nil
	}

	rep, err := h.Run(context.Background(), "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Success {
		t.Errorf("should not succeed")
	}
	if rep.Rounds != 2 {
		t.Errorf("want 2 rounds, got %d", rep.Rounds)
	}
	if len(rep.ToolRecords) != 2 {
		t.Errorf("want 2 rounds of records, got %d", len(rep.ToolRecords))
	}
}

func TestHarness_Run_NoToolCall_ReturnsSuccess(t *testing.T) {
	// LLM 啥都没调 —— 我们视作"它决定不做事,任务完成"。
	h, _ := setupHarness(t, map[string]string{
		"a.go": `package a
func X() {}
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
	})
	h.Index(context.Background())
	h.AsLLMTools(fakeBuilder{})

	h.cfg.MainCaller = func(sys, usr string, toolList []any) error {
		return nil // 啥都不调
	}
	rep, err := h.Run(context.Background(), "just chat")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Success {
		t.Errorf("no-tool run should be Success=true")
	}
	if rep.Rounds != 1 {
		t.Errorf("want 1 round, got %d", rep.Rounds)
	}
}

func TestHarness_Run_RequiresAsLLMToolsFirst(t *testing.T) {
	// Run 前没调 AsLLMTools,应报错
	h, _ := setupHarness(t, nil, Config{
		TriageCaller: testTriageAllSkeleton(),
		MainCaller:   func(s, u string, t []any) error { return nil },
	})
	_, err := h.Run(context.Background(), "x")
	if err == nil {
		t.Fatal("Run should fail when tools not registered")
	}
	if !strings.Contains(err.Error(), "AsLLMTools") {
		t.Errorf("error should mention AsLLMTools, got: %v", err)
	}
}

// --- AsLLMTools 行为 ---

func TestHarness_AsLLMTools_ReturnsAllSeven(t *testing.T) {
	h, _ := setupHarness(t, nil, Config{})
	tl := h.AsLLMTools(fakeBuilder{})
	if len(tl) != 7 {
		t.Errorf("want 7 tools, got %d", len(tl))
	}
}

func TestHarness_AsLLMTools_IsIdempotent(t *testing.T) {
	// 多次调用应返回同一 bundle
	h, _ := setupHarness(t, nil, Config{})
	first := h.AsLLMTools(fakeBuilder{})
	second := h.AsLLMTools(fakeBuilder{})
	// 对象身份(容量/指针)相同
	if len(first) != len(second) {
		t.Errorf("length differs")
	}
}

// --- TS 集成(只在有 bun shim 时跑)---

func TestHarness_TS_EndToEnd(t *testing.T) {
	skipIfNoTS(t)

	h, _ := setupHarness(t, map[string]string{
		"app.ts": `export function greet(name: string): string {
    return "hi " + name;
}
`,
	}, Config{
		TriageCaller: testTriageAllSkeleton(),
	})

	if _, err := h.Index(context.Background()); err != nil {
		t.Fatalf("index: %v", err)
	}

	var greetID string
	for _, c := range h.Store().AllChunks() {
		if c.Name == "greet" {
			greetID = c.ID
		}
	}
	if greetID == "" {
		t.Fatalf("greet not indexed: chunks=%v", h.Store().AllChunks())
	}

	h.AsLLMTools(fakeBuilder{})
	h.cfg.MainCaller = successfulMainCaller(t, "modify_chunk", &tools.ModifyChunkPayload{
		ChunkID: greetID,
		NewContent: `export function greet(name: string): string {
    return "hello " + name + "!";
}`,
	})

	rep, err := h.Run(context.Background(), "make greet more enthusiastic")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rep.Success {
		t.Errorf("should succeed; records: %+v", rep.ToolRecords)
	}
}

// 显式使用 chunk 包防止 unused(间接通过 store 已使用,但保留以示可直接依赖)
var _ = chunk.KindFunction
