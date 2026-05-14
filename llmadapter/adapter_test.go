// adapter_test.go — llmadapter 的单元测试。
//
// 这一层不去戳真实 LLM (stub 的 Call 是 no-op),覆盖范围是 wiring 的正确性:
//
//   - Models 的 fallback 链:Main → Triage → Expand
//   - CallerSet 字段都非空
//   - DefaultTriageTemplate / DefaultExpandTemplate 是合法的 text/template,
//     并且能渲染包含 chunk.Kind / Name / Signature 的真实 Views
//   - NewToolBuilder 对 dopharness 已知的所有 Payload 类型都返回非空工具
//   - NewToolBuilder 对未知 payload 类型 panic (fail-fast 契约)
//   - QuickConfig 的 validate 拒绝缺字段、缺模型
//   - QuickStart 返回的 tools 列表包含 task_reflect (memory tools 默认开启)
//   - DisableMemoryTools 时 tools 列表不含 task_reflect
//   - NewMainCaller 把非 llm.ToolInterface 的 tools 元素拒绝并报错(类型安全)
//
// 由于 stub 的 agent.Call() 总是返回 nil,我们不验证"LLM 真正生成了正确的
// TriageDecisionPayload",那是集成测试的范畴。本文件只验证"调用方不会被
// 错误的 wiring 绊倒"。

package llmadapter

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/doptime/llm"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/gateway"
	"github.com/doptime/dopharness/harness"
	"github.com/doptime/dopharness/tools"
)

// ============================================================
// Models 的 fallback 链
// ============================================================

func TestModels_ResolveFallbackChain(t *testing.T) {
	cases := []struct {
		name string
		in   Models
		want Models
	}{
		{
			name: "only Main, others fallback",
			in:   Models{Main: llm.Qwen36_35ba3b},
			want: Models{
				Main:   llm.Qwen36_35ba3b,
				Triage: llm.Qwen36_35ba3b,
				Expand: llm.Qwen36_35ba3b,
			},
		},
		{
			name: "Main + Triage, Expand falls back to Triage",
			in:   Models{Main: llm.Qwen3627b, Triage: llm.Qwen36_35ba3b},
			want: Models{
				Main:   llm.Qwen3627b,
				Triage: llm.Qwen36_35ba3b,
				Expand: llm.Qwen36_35ba3b,
			},
		},
		{
			name: "all three set, no fallback",
			in: Models{
				Main:   llm.Qwen3627b,
				Triage: llm.Qwen36_35ba3b,
				Expand: llm.Qwen36_35ba3b,
			},
			want: Models{
				Main:   llm.Qwen3627b,
				Triage: llm.Qwen36_35ba3b,
				Expand: llm.Qwen36_35ba3b,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.resolve()
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// ============================================================
// CallerSet 完整性
// ============================================================

func TestDefault_FillsAllCallerSetFields(t *testing.T) {
	cs := Default(llm.Qwen36_35ba3b)
	if cs == nil {
		t.Fatal("Default returned nil")
	}
	if cs.Triage == nil {
		t.Error("Triage caller is nil")
	}
	if cs.Expand == nil {
		t.Error("Expand caller is nil")
	}
	if cs.Main == nil {
		t.Error("Main caller is nil")
	}
	if cs.ToolBuilder == nil {
		t.Error("ToolBuilder is nil")
	}
}

func TestMultiModel_AppliesFallbackBeforeBuilding(t *testing.T) {
	// 故意只填 Main,确保 MultiModel 内部 resolve 后 Triage/Expand 也能建出 caller
	cs := MultiModel(Models{Main: llm.Qwen36_35ba3b})
	if cs.Triage == nil || cs.Expand == nil {
		t.Error("MultiModel didn't fallback-resolve before building")
	}
}

// ============================================================
// 默认模板 — 必须是合法 text/template,且能渲染真实 Views
// ============================================================

func TestDefaultTriageTemplate_RendersRealViews(t *testing.T) {
	tpl, err := template.New("t").Parse(DefaultTriageTemplate)
	if err != nil {
		t.Fatalf("DefaultTriageTemplate doesn't parse: %v", err)
	}

	views := []*gateway.ChunkView{
		{
			ID: "a1b2", Kind: chunk.KindFunction, Name: "ComputeScore",
			FilePath:  "pkg/score/score.go",
			Signature: "func ComputeScore(hint bool) int {",
			Refs:      []string{"intl.NumberFormat"},
		},
		{
			ID: "c3d4", Kind: chunk.KindStruct, Name: "GameState",
			FilePath:  "pkg/state/state.go",
			Signature: "type GameState struct {",
		},
	}

	var buf bytes.Buffer
	err = tpl.Execute(&buf, map[string]any{
		"UserPrompt": "fix the scoring bug",
		"Views":      views,
	})
	if err != nil {
		t.Fatalf("template execute: %v", err)
	}
	out := buf.String()

	// 用户任务应当被引用
	if !strings.Contains(out, "fix the scoring bug") {
		t.Errorf("rendered template missing user prompt:\n%s", out)
	}
	// 每个 chunk 至少出现 ID 一次
	for _, v := range views {
		if !strings.Contains(out, v.ID) {
			t.Errorf("rendered template missing chunk ID %s:\n%s", v.ID, out)
		}
		if !strings.Contains(out, v.Name) {
			t.Errorf("rendered template missing chunk name %s:\n%s", v.Name, out)
		}
	}
	// 关键裁决词必须出现 (LLM 据此理解约定)
	for _, keyword := range []string{"IGNORE", "SKELETON", "FULL", "TriageDecision"} {
		if !strings.Contains(out, keyword) {
			t.Errorf("rendered template missing keyword %q:\n%s", keyword, out)
		}
	}
}

func TestDefaultExpandTemplate_RendersRealSkeletons(t *testing.T) {
	tpl, err := template.New("e").Parse(DefaultExpandTemplate)
	if err != nil {
		t.Fatalf("DefaultExpandTemplate doesn't parse: %v", err)
	}

	skeletons := []*gateway.SkeletonItem{
		{
			ID: "ef56", Kind: chunk.KindMethod, Name: "Apply",
			FilePath:  "edit/apply.go",
			Signature: "func (a *Applier) Apply(m *Modification) *ApplyResult {",
		},
	}

	var buf bytes.Buffer
	err = tpl.Execute(&buf, map[string]any{
		"UserPrompt": "refactor apply pipeline",
		"Skeletons":  skeletons,
	})
	if err != nil {
		t.Fatalf("expand template execute: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "ef56") {
		t.Errorf("expand template missing skeleton ID:\n%s", out)
	}
	if !strings.Contains(out, "ExpandDecision") {
		t.Errorf("expand template missing tool name 'ExpandDecision':\n%s", out)
	}
	if !strings.Contains(out, "FULL") {
		t.Errorf("expand template should mention 'FULL' (upgrade target):\n%s", out)
	}
}

// ============================================================
// NewToolBuilder — 必须覆盖所有 dopharness 已知 payload 类型
// ============================================================

func TestNewToolBuilder_CoversAllEditingPayloads(t *testing.T) {
	b := NewToolBuilder()

	// 用一个最小 ToolBuilder 测试中的"假"handler:对每种 payload 类型,
	// 构造一个 func(*P) 然后调 b.Build,期望返回值是 llm.ToolInterface。
	cases := []struct {
		name    string
		handler any
	}{
		{"modify_chunk", func(*tools.ModifyChunkPayload) {}},
		{"delete_chunk", func(*tools.DeleteChunkPayload) {}},
		{"add_chunk", func(*tools.AddChunkPayload) {}},
		{"create_file", func(*tools.CreateFilePayload) {}},
		{"delete_file", func(*tools.DeleteFilePayload) {}},
		{"task_reflect", func(*harness.TaskReflectPayload) {}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := b.Build(c.name, "test description", c.handler)
			if got == nil {
				t.Fatalf("Build returned nil for %s", c.name)
			}
			it, ok := got.(llm.ToolInterface)
			if !ok {
				t.Fatalf("Build returned %T, not llm.ToolInterface", got)
			}
			if it.Name() != c.name {
				t.Errorf("ToolName: want %q, got %q", c.name, it.Name())
			}
		})
	}
}

func TestNewToolBuilder_PanicsOnUnknownPayload(t *testing.T) {
	b := NewToolBuilder()
	type unknownPayload struct{ X int }

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on unknown payload type")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", r)
		}
		if !strings.Contains(msg, "unknown tool handler type") {
			t.Errorf("panic message should mention unknown handler, got: %s", msg)
		}
	}()
	b.Build("bogus", "should panic", func(*unknownPayload) {})
}

// ============================================================
// NewMainCaller — 类型断言失败应当返回 error,不要 panic
// ============================================================

func TestNewMainCaller_RejectsNonLLMTool(t *testing.T) {
	caller := NewMainCaller(llm.Qwen36_35ba3b)

	// 给一个明显不是 llm.ToolInterface 的东西,期望 caller 报错
	bogusTools := []any{"i am a string, not a tool"}
	err := caller("sys", "user", bogusTools)
	if err == nil {
		t.Fatal("expected error from non-tool-interface element")
	}
	if !strings.Contains(err.Error(), "not llm.ToolInterface") {
		t.Errorf("error should mention type mismatch, got: %v", err)
	}
}

func TestNewMainCaller_AcceptsProperToolInterface(t *testing.T) {
	caller := NewMainCaller(llm.Qwen36_35ba3b)

	// 用 NewToolBuilder 造的工具应该被接受 (它们一定是 llm.ToolInterface)
	b := NewToolBuilder()
	tl := []any{
		b.Build("modify_chunk", "desc", func(*tools.ModifyChunkPayload) {}),
		b.Build("task_reflect", "desc", func(*harness.TaskReflectPayload) {}),
	}
	// stub 的 Call 不报错,我们只验证 caller 本身不报错
	if err := caller("sys", "user", tl); err != nil {
		t.Errorf("unexpected error from valid tool list: %v", err)
	}
}

// ============================================================
// joinPrompts — 边界
// ============================================================

func TestJoinPrompts(t *testing.T) {
	cases := []struct {
		sys, usr, want string
	}{
		{"sys", "usr", "sys\n\nusr"},
		{"", "usr", "usr"},
		{"sys", "", "sys"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := joinPrompts(c.sys, c.usr); got != c.want {
			t.Errorf("joinPrompts(%q, %q) = %q, want %q", c.sys, c.usr, got, c.want)
		}
	}
}

// ============================================================
// QuickConfig.validate
// ============================================================

func TestQuickConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     QuickConfig
		wantErr string // substring expected in err.Error(); "" = no error
	}{
		{
			name:    "missing ProjectRoot",
			cfg:     QuickConfig{Model: llm.Qwen36_35ba3b},
			wantErr: "ProjectRoot is required",
		},
		{
			name:    "no model at all",
			cfg:     QuickConfig{ProjectRoot: "."},
			wantErr: "Model or Models.Main",
		},
		{
			name:    "just Model is OK",
			cfg:     QuickConfig{ProjectRoot: ".", Model: llm.Qwen36_35ba3b},
			wantErr: "",
		},
		{
			name:    "just Models.Main is OK",
			cfg:     QuickConfig{ProjectRoot: ".", Models: Models{Main: llm.Qwen36_35ba3b}},
			wantErr: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.validate()
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("want no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("want error containing %q, got nil", c.wantErr)
				} else if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("want err containing %q, got %v", c.wantErr, err)
				}
			}
		})
	}
}

func TestQuickConfig_ResolveModels_ModelsTakesPrecedenceOverModel(t *testing.T) {
	cfg := QuickConfig{
		ProjectRoot: ".",
		Model:       llm.Qwen36_35ba3b,           // 单模型字段
		Models:      Models{Main: llm.Qwen3627b}, // 多模型字段 — 应胜出
	}
	m := cfg.resolveModels()
	if m.Main != llm.Qwen3627b {
		t.Errorf("Models.Main should override Model field, got Main=%v", m.Main)
	}
}

func TestQuickConfig_ResolveModels_FallbackFromModel(t *testing.T) {
	cfg := QuickConfig{
		ProjectRoot: ".",
		Model:       llm.Qwen36_35ba3b,
	}
	m := cfg.resolveModels()
	if m.Main != llm.Qwen36_35ba3b || m.Triage != llm.Qwen36_35ba3b || m.Expand != llm.Qwen36_35ba3b {
		t.Errorf("Model should fill all roles, got %+v", m)
	}
}

// ============================================================
// QuickStart — end-to-end wiring
// ============================================================

func TestQuickStart_BuildsHarnessWithMemoryToolsByDefault(t *testing.T) {
	dir := t.TempDir()
	h, tl, err := QuickStart(QuickConfig{
		ProjectRoot: dir,
		Model:       llm.Qwen36_35ba3b,
	})
	if err != nil {
		t.Fatalf("QuickStart failed: %v", err)
	}
	if h == nil {
		t.Fatal("returned harness is nil")
	}

	// 默认应当启用 memory tools → tools 列表里有 task_reflect
	if !containsToolNamed(tl, "task_reflect") {
		t.Errorf("default QuickStart should include task_reflect tool, got %d tools (none named task_reflect)",
			len(tl))
	}
	// 也应当包含 5 个 edit tools
	for _, name := range []string{"modify_chunk", "delete_chunk", "add_chunk", "create_file", "delete_file"} {
		if !containsToolNamed(tl, name) {
			t.Errorf("default QuickStart missing edit tool %q", name)
		}
	}
}

func TestQuickStart_DisableMemoryToolsRespected(t *testing.T) {
	dir := t.TempDir()
	_, tl, err := QuickStart(QuickConfig{
		ProjectRoot:        dir,
		Model:              llm.Qwen36_35ba3b,
		DisableMemoryTools: true,
	})
	if err != nil {
		t.Fatalf("QuickStart failed: %v", err)
	}
	if containsToolNamed(tl, "task_reflect") {
		t.Error("DisableMemoryTools=true should exclude task_reflect")
	}
}

func TestQuickStart_ProjectRootRequired(t *testing.T) {
	_, _, err := QuickStart(QuickConfig{Model: llm.Qwen36_35ba3b})
	if err == nil {
		t.Fatal("expected error for missing ProjectRoot")
	}
}

func TestQuickStart_ModelRequired(t *testing.T) {
	dir := t.TempDir()
	_, _, err := QuickStart(QuickConfig{ProjectRoot: dir})
	if err == nil {
		t.Fatal("expected error for missing Model and Models.Main")
	}
}

func TestQuickStart_LoggerIsWired(t *testing.T) {
	dir := t.TempDir()
	var events []string
	logger := func(event string, _ map[string]any) {
		events = append(events, event)
	}
	h, _, err := QuickStart(QuickConfig{
		ProjectRoot: dir,
		Model:       llm.Qwen36_35ba3b,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("QuickStart: %v", err)
	}
	// 触发一个会产生事件的操作:WithMemoryTools 在构造时会发 memory_tools_registered。
	// 这一事件已在 QuickStart 里发生过了 (DisableMemoryTools=false),所以应在 events 里。
	found := false
	for _, e := range events {
		if e == "memory_tools_registered" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Logger should have captured memory_tools_registered event, got: %v", events)
	}
	_ = h // 防止"declared and not used"
}

// ============================================================
// 辅助
// ============================================================

// containsToolNamed 检查工具列表里有没有指定名字的工具。
// 工具是 llm.ToolInterface (来自 stub),通过 ToolName() 方法判断。
func containsToolNamed(toolList []any, name string) bool {
	for _, t := range toolList {
		it, ok := t.(llm.ToolInterface)
		if !ok {
			continue
		}
		if it.Name() == name {
			return true
		}
	}
	return false
}

// 编译期检查:CallerSet 的字段类型与 dopharness 的接口对得上。
// 如果将来 dopharness 改了任一接口签名,这里会立刻编译失败,
// 提醒 llmadapter 同步更新。
//
// 用零值 CallerSet (而不是 nil 指针解引) 避免 init 时 panic ——
// 零值的字段都是 nil 函数,但类型仍然可被赋给接口变量。
var (
	_cs                      = CallerSet{}
	_   gateway.TriageCaller = _cs.Triage
	_   gateway.ExpandCaller = _cs.Expand
	_   harness.MainCaller   = _cs.Main
	_   tools.ToolBuilder    = _cs.ToolBuilder
)
