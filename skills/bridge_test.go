package skills

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mockRunner 把每次调用的参数记下,方便测试断言。
type mockRunner struct {
	mu       sync.Mutex
	calls    []mockRunnerCall
	returnFn func(ctx context.Context, prompt, name string) error
}

type mockRunnerCall struct {
	ctx       context.Context
	prompt    string
	skillName string
}

func (m *mockRunner) run() SubAgentRunner {
	return func(ctx context.Context, prompt string, name string) error {
		m.mu.Lock()
		m.calls = append(m.calls, mockRunnerCall{ctx, prompt, name})
		m.mu.Unlock()
		if m.returnFn != nil {
			return m.returnFn(ctx, prompt, name)
		}
		return nil
	}
}

// 端到端: LLM 提交 JSON → DynamicTool 反序列化 → bridge sink 渲染 SOP →
// runner 收到带 substituted 字段的 prompt + skillName。
func TestSkill_AsLLMTool_EndToEnd(t *testing.T) {
	skills, err := LoadSkillFile(filepath.Join("testdata", "valid_skill.go"))
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	var add *Skill
	for _, s := range skills {
		if s.Name == "AddRoute" {
			add = s
			break
		}
	}
	if add == nil {
		t.Fatal("AddRoute skill 未找到")
	}

	mock := &mockRunner{}
	tool := add.AsLLMTool(mock.run())
	if tool == nil {
		t.Fatal("AsLLMTool 返回 nil")
	}
	if tool.Name() != "AddRoute" {
		t.Errorf("tool.Name() = %q, want AddRoute", tool.Name())
	}

	// 模拟 LLM 提交的 JSON 参数
	rawArgs := `{
		"path": "/api/foo",
		"handler_name": "fooHandler",
		"method": "GET",
		"tags": ["api", "v1"]
	}`
	cm := map[string]any{}
	if err := tool.HandleCallback(rawArgs, cm); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("runner 被调用 %d 次,期望 1 次", len(mock.calls))
	}
	got := mock.calls[0]
	if got.skillName != "AddRoute" {
		t.Errorf("skillName = %q, want AddRoute", got.skillName)
	}
	// 渲染后的 prompt 必须包含填进来的字段
	for _, want := range []string{"/api/foo", "fooHandler", "GET", "[api v1]"} {
		if !strings.Contains(got.prompt, want) {
			t.Errorf("prompt 缺少 %q;实际:\n%s", want, got.prompt)
		}
	}

	// 字段反向落到 CallMemory(NewToolFromType 自动做的)
	if cm["path"] != "/api/foo" {
		t.Errorf("CallMemory[path] = %v, want /api/foo", cm["path"])
	}
}

// runner 返回 error 应该被当作 toolcall 失败往上传。
func TestSkill_AsLLMTool_RunnerErrorPropagates(t *testing.T) {
	skills, err := LoadSkillFile(filepath.Join("testdata", "valid_skill.go"))
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	var add *Skill
	for _, s := range skills {
		if s.Name == "AddRoute" {
			add = s
		}
	}

	wantErr := errors.New("sub-agent 模拟失败")
	mock := &mockRunner{
		returnFn: func(ctx context.Context, prompt, name string) error {
			return wantErr
		},
	}
	tool := add.AsLLMTool(mock.run())
	err = tool.HandleCallback(`{"path":"/x","handler_name":"h","method":"GET"}`, nil)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
}

// runner == nil 时应退化为报错的 sink(尽早暴露配置漏配)。
func TestSkill_AsLLMTool_NilRunner(t *testing.T) {
	skills, err := LoadSkillFile(filepath.Join("testdata", "valid_skill.go"))
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	tool := skills[0].AsLLMTool(nil)
	err = tool.HandleCallback(`{"path":"/x","handler_name":"h"}`, nil)
	if err == nil {
		t.Error("nil runner 时 HandleCallback 应该报错")
	}
}

// CallMemory 里的 Context 应该被 sink 透传给 runner。
func TestSkill_AsLLMTool_ContextPropagation(t *testing.T) {
	skills, err := LoadSkillFile(filepath.Join("testdata", "valid_skill.go"))
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	var add *Skill
	for _, s := range skills {
		if s.Name == "AddRoute" {
			add = s
		}
	}

	type ctxKey struct{}
	parent := context.WithValue(context.Background(), ctxKey{}, "parent-tag")

	mock := &mockRunner{}
	tool := add.AsLLMTool(mock.run())
	cm := map[string]any{"Context": parent}
	if err := tool.HandleCallback(`{"path":"/x","handler_name":"h"}`, cm); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if len(mock.calls) != 1 {
		t.Fatal("runner 未被调用")
	}
	got := mock.calls[0].ctx.Value(ctxKey{})
	if got != "parent-tag" {
		t.Errorf("ctx 没有透传父 context,值 = %v", got)
	}
}

// Registry.AsLLMTools 一次性把整个 registry 转成 ToolInterface 切片。
func TestRegistry_AsLLMTools(t *testing.T) {
	r := NewRegistry()
	if err := r.LoadDir("testdata"); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	mock := &mockRunner{}
	tools := r.AsLLMTools(mock.run())
	if len(tools) != 2 {
		t.Errorf("got %d tools, want 2", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	if !names["AddRoute"] || !names["RefactorFunction"] {
		t.Errorf("tool names = %v, want AddRoute + RefactorFunction", names)
	}
}
