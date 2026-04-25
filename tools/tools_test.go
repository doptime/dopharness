package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/edit"
	"github.com/doptime/dopharness/store"
)

// fakeTool 是我们测试用的"假"工具对象:记下 name、desc、handler 供 Tool-level 断言。
type fakeTool struct {
	name        string
	description string
	handler     any
}

// fakeBuilder 把每次 Build 调用的参数保留下来,并返回一个能直接调 handler 的对象。
type fakeBuilder struct {
	built []*fakeTool
}

func (fb *fakeBuilder) Build(name, description string, handler any) any {
	t := &fakeTool{name: name, description: description, handler: handler}
	fb.built = append(fb.built, t)
	return t
}

// invoke 模拟 LLM 调工具:按 name 找到 fakeTool,然后把 payload 传给 handler。
// 在真实环境里 llm 库会通过反射从 JSON 反序列化得到 payload 并调用。
func invoke(t *testing.T, tools []any, name string, payload any) {
	t.Helper()
	for _, x := range tools {
		ft := x.(*fakeTool)
		if ft.name == name {
			// handler 是 func(*T);用反射即可,但为了测试简单,这里做类型分支
			switch h := ft.handler.(type) {
			case func(*ModifyChunkPayload):
				h(payload.(*ModifyChunkPayload))
			case func(*DeleteChunkPayload):
				h(payload.(*DeleteChunkPayload))
			case func(*AddChunkPayload):
				h(payload.(*AddChunkPayload))
			case func(*CreateFilePayload):
				h(payload.(*CreateFilePayload))
			case func(*DeleteFilePayload):
				h(payload.(*DeleteFilePayload))
			case func(*ReadChunkPayload):
				h(payload.(*ReadChunkPayload))
			case func(*SearchChunksByNamePayload):
				h(payload.(*SearchChunksByNamePayload))
			default:
				t.Fatalf("unknown handler type for %s: %T", name, ft.handler)
			}
			return
		}
	}
	t.Fatalf("tool %q not found in bundle", name)
}

// setupToolsEnv 建一个带 Go 索引的 env。
func setupToolsEnv(t *testing.T, files map[string]string) (*Bundle, string, store.ChunkStore) {
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
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	// 先索引 Go 文件
	for rel := range files {
		if strings.HasSuffix(rel, ".go") {
			abs := filepath.Join(root, rel)
			cs, err := chunk.ParseGoFile(abs, rel)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(abs)
			stat, _ := os.Stat(abs)
			_, err = st.UpsertFile(rel, chunk.HashBody(string(data)), stat.ModTime().Unix(), cs)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	validator := edit.NewValidator(nil) // 不测 TS,就不接 TS validator
	ap := edit.NewApplier(root, st, validator)
	ap.GoParse = chunk.ParseGoFile

	builder := &fakeBuilder{}
	bundle := BuildEditingTools(ap, builder)
	return bundle, root, st
}

// --- 工具注册结构断言 ---

func TestBuildEditingTools_AllSevenToolsRegistered(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{})
	if len(bundle.Tools) != 7 {
		t.Errorf("want 7 tools, got %d", len(bundle.Tools))
	}
	wantNames := []string{
		"modify_chunk", "delete_chunk", "add_chunk",
		"create_file", "delete_file",
		"read_chunk", "search_chunks_by_name",
	}
	gotNames := map[string]bool{}
	for _, x := range bundle.Tools {
		gotNames[x.(*fakeTool).name] = true
	}
	for _, n := range wantNames {
		if !gotNames[n] {
			t.Errorf("tool %q not registered", n)
		}
	}
}

// --- modify_chunk 成功路径 ---

func TestTool_ModifyChunk_Success(t *testing.T) {
	bundle, _, st := setupToolsEnv(t, map[string]string{
		"a.go": `package a

func Foo() int {
	return 1
}
`,
	})
	var fooID string
	for _, c := range st.AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	invoke(t, bundle.Tools, "modify_chunk", &ModifyChunkPayload{
		ChunkID:    fooID,
		NewContent: "func Foo() int { return 42 }",
	})

	records := bundle.Collector.Records()
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	if !records[0].OK() {
		t.Errorf("modify should succeed: %+v", records[0].Result)
	}
	if !bundle.Collector.AllOK() {
		t.Error("collector.AllOK should be true")
	}
}

// --- modify_chunk 失败路径(错误 ID)---

func TestTool_ModifyChunk_WrongID_RecordedFailure(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{
		"a.go": `package a
func Foo() {}
`,
	})

	invoke(t, bundle.Tools, "modify_chunk", &ModifyChunkPayload{
		ChunkID:    "xXxX",
		NewContent: "func Foo() {}",
	})

	records := bundle.Collector.Records()
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	if records[0].OK() {
		t.Errorf("should have failed, got: %+v", records[0].Result)
	}
	if !bundle.Collector.AnyFailure() {
		t.Error("collector.AnyFailure should be true")
	}

	// 失败时,BuildSummary 里应包含 FAILED 和原因
	summary := bundle.Collector.BuildSummary()
	if !strings.Contains(summary, "FAILED") {
		t.Errorf("summary should mark failure:\n%s", summary)
	}
	if !strings.Contains(summary, "modify_chunk") {
		t.Errorf("summary should name the tool:\n%s", summary)
	}
}

// --- create_file / delete_file / add_chunk / delete_chunk 各自的端到端 ---

func TestTool_CreateFile_Success(t *testing.T) {
	bundle, root, st := setupToolsEnv(t, map[string]string{})
	invoke(t, bundle.Tools, "create_file", &CreateFilePayload{
		FilePath:   "new/mod.go",
		NewContent: "package mod\n\nfunc Hi() {}\n",
	})
	records := bundle.Collector.Records()
	if !records[0].OK() {
		t.Fatalf("create_file failed: %s", records[0].Result.Message)
	}
	if _, err := os.Stat(filepath.Join(root, "new/mod.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
	if len(st.ChunksByFile("new/mod.go")) != 1 {
		t.Errorf("index not updated")
	}
}

func TestTool_DeleteFile_Success(t *testing.T) {
	bundle, root, _ := setupToolsEnv(t, map[string]string{
		"a.go": `package a
func X() {}
`,
	})
	invoke(t, bundle.Tools, "delete_file", &DeleteFilePayload{FilePath: "a.go"})
	if _, err := os.Stat(filepath.Join(root, "a.go")); !os.IsNotExist(err) {
		t.Errorf("file should be gone")
	}
}

func TestTool_AddChunk_Success(t *testing.T) {
	bundle, root, _ := setupToolsEnv(t, map[string]string{
		"a.go": `package a

func Foo() {}
`,
	})
	invoke(t, bundle.Tools, "add_chunk", &AddChunkPayload{
		FilePath:   "a.go",
		NewContent: "func Bar() {}",
	})
	records := bundle.Collector.Records()
	if !records[0].OK() {
		t.Fatalf("add_chunk failed: %s", records[0].Result.Message)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(data), "func Bar()") {
		t.Errorf("added chunk not in file:\n%s", data)
	}
}

func TestTool_DeleteChunk_Success(t *testing.T) {
	bundle, _, st := setupToolsEnv(t, map[string]string{
		"a.go": `package a

func Foo() {}

func Bar() {}
`,
	})
	var fooID string
	for _, c := range st.AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}
	invoke(t, bundle.Tools, "delete_chunk", &DeleteChunkPayload{ChunkID: fooID})
	records := bundle.Collector.Records()
	if !records[0].OK() {
		t.Fatalf("delete_chunk failed: %s", records[0].Result.Message)
	}
	if len(st.ChunksByFile("a.go")) != 1 {
		t.Errorf("want 1 chunk left, got %d", len(st.ChunksByFile("a.go")))
	}
}

// --- read_chunk 只读工具 ---

func TestTool_ReadChunk_Hit(t *testing.T) {
	bundle, _, st := setupToolsEnv(t, map[string]string{
		"a.go": `package a
func Foo() int { return 1 }
`,
	})
	var fooID string
	for _, c := range st.AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}
	invoke(t, bundle.Tools, "read_chunk", &ReadChunkPayload{ChunkID: fooID})
	records := bundle.Collector.Records()
	if !records[0].OK() {
		t.Fatalf("read_chunk should succeed")
	}
	if !strings.Contains(records[0].Result.Message, "return 1") {
		t.Errorf("body missing in message:\n%s", records[0].Result.Message)
	}
	if !strings.Contains(records[0].Result.Message, fooID) {
		t.Errorf("ID missing in message")
	}
}

func TestTool_ReadChunk_Miss(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{})
	invoke(t, bundle.Tools, "read_chunk", &ReadChunkPayload{ChunkID: "zzzz"})
	records := bundle.Collector.Records()
	if records[0].OK() {
		t.Errorf("should fail when chunk missing")
	}
	if records[0].Result.Outcome != edit.OutcomeLocation {
		t.Errorf("outcome want Location, got %s", records[0].Result.Outcome)
	}
}

// --- search_chunks_by_name ---

func TestTool_Search_MultiMatch(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{
		"a.go": `package a
func Helper() {}
`,
		"b.go": `package b
func Helper() {}
`,
	})
	invoke(t, bundle.Tools, "search_chunks_by_name", &SearchChunksByNamePayload{Name: "Helper"})
	records := bundle.Collector.Records()
	if !records[0].OK() {
		t.Fatalf("search should succeed")
	}
	if !strings.Contains(records[0].Result.Message, "2 matches") {
		t.Errorf("count missing:\n%s", records[0].Result.Message)
	}
	if len(records[0].Result.AffectedIDs) != 2 {
		t.Errorf("want 2 IDs, got %d", len(records[0].Result.AffectedIDs))
	}
}

func TestTool_Search_NoMatch(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{})
	invoke(t, bundle.Tools, "search_chunks_by_name", &SearchChunksByNamePayload{Name: "Nothing"})
	records := bundle.Collector.Records()
	if records[0].OK() {
		t.Errorf("should fail for no-match")
	}
}

// --- Collector 行为 ---

func TestCollector_Reset(t *testing.T) {
	bundle, _, _ := setupToolsEnv(t, map[string]string{})
	invoke(t, bundle.Tools, "create_file", &CreateFilePayload{
		FilePath: "x.go", NewContent: "package x\nfunc X(){}",
	})
	if len(bundle.Collector.Records()) != 1 {
		t.Fatalf("want 1 record before reset")
	}
	bundle.Collector.Reset()
	if len(bundle.Collector.Records()) != 0 {
		t.Errorf("want 0 records after reset")
	}
	if bundle.Collector.AllOK() {
		t.Error("empty collector AllOK should be false")
	}
}
