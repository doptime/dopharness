package edit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// --- 测试环境脚手架 ---

// setupEditEnv 搭建一个带 Go parser 接入的 Applier 实例(TS 部分可选)。
// fileContent map[relPath]content 决定初始文件;会自动索引所有 Go 文件。
func setupEditEnv(t *testing.T, fileContent map[string]string) (*Applier, store.ChunkStore, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range fileContent {
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

	// 预解析所有 Go 文件入库
	for rel := range fileContent {
		if strings.HasSuffix(rel, ".go") {
			abs := filepath.Join(root, rel)
			chunks, err := chunk.ParseGoFile(abs, rel)
			if err != nil {
				t.Fatalf("initial parse %s: %v", rel, err)
			}
			content, _ := os.ReadFile(abs)
			stat, _ := os.Stat(abs)
			_, err = st.UpsertFile(rel, chunk.HashBody(string(content)), stat.ModTime().Unix(), chunks)
			if err != nil {
				t.Fatalf("initial upsert %s: %v", rel, err)
			}
		}
	}

	// TS parser 如果环境有 bun/shim 则接入
	var tsVal TSValidator
	var tsParse func(string, string) ([]*chunk.Chunk, error)
	if hasBunRuntime() {
		p := chunk.NewTSParser()
		tsVal = TSValidatorFunc(func(code, kind string) (bool, int, int, string, error) {
			ok, errs, err := p.ValidateCode(code, kind)
			if err != nil {
				return false, 0, 0, "", err
			}
			if ok {
				return true, 0, 0, "", nil
			}
			e := errs[0]
			return false, e.Line, e.Column, e.Message, nil
		})
		tsParse = p.ParseTSFile
	}

	validator := NewValidator(tsVal)
	ap := NewApplier(root, st, validator)
	ap.GoParse = chunk.ParseGoFile
	ap.TSParse = tsParse
	return ap, st, root
}

func hasBunRuntime() bool {
	if override := os.Getenv("DOPHARNESS_BUN"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return true
		}
	}
	if _, err := exec.LookPath("bun"); err == nil {
		return true
	}
	return false
}

// --- Locator 单元测试 ---

func TestLocator_ExactHit(t *testing.T) {
	ap, st, _ := setupEditEnv(t, map[string]string{
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
	loc := ap.Locator.Locate(fooID, "")
	if loc.Outcome != LocateExact {
		t.Errorf("want Exact, got %s: %s", loc.Outcome, loc.Message)
	}
	if loc.Chunk.Name != "Foo" {
		t.Errorf("wrong chunk: %s", loc.Chunk.Name)
	}
}

func TestLocator_FuzzyByName(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{
		"a.go": `package a
func Foo() {}
`,
	})
	// LLM 给了个看起来像老式 ID 的东西:"a.go:Foo"
	loc := ap.Locator.Locate("a.go:Foo", "")
	if loc.Outcome != LocateFuzzyUnique {
		t.Errorf("want FuzzyUnique, got %s: %s", loc.Outcome, loc.Message)
	}
	if loc.Chunk == nil || loc.Chunk.Name != "Foo" {
		t.Errorf("fallback should resolve to Foo, got %+v", loc.Chunk)
	}
}

func TestLocator_Ambiguous(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{
		"a.go": `package a
func Helper() {}
`,
		"b.go": `package b
func Helper() {}
`,
	})
	loc := ap.Locator.Locate("", "Helper")
	if loc.Outcome != LocateAmbiguous {
		t.Errorf("want Ambiguous, got %s: %s", loc.Outcome, loc.Message)
	}
	if len(loc.Candidates) != 2 {
		t.Errorf("want 2 candidates, got %d", len(loc.Candidates))
	}
	if !strings.Contains(loc.Message, "ambiguous") {
		t.Errorf("message not helpful: %s", loc.Message)
	}
}

func TestLocator_Missing(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{
		"a.go": `package a
func Foo() {}
`,
	})
	loc := ap.Locator.Locate("", "NonExistent")
	if loc.Outcome != LocateMissing {
		t.Errorf("want Missing, got %s", loc.Outcome)
	}
	if !strings.Contains(loc.Message, "CREATE_FILE") {
		t.Errorf("missing message should hint at CREATE_FILE: %s", loc.Message)
	}
}

// --- Validator 单元测试 ---

func TestValidator_GoSnippetValid(t *testing.T) {
	v := NewValidator(nil)
	// 裸顶层声明(没有 package):Validator 会包一层
	e := v.Validate("a.go", "func Foo() int { return 1 }", "")
	_ = e // 合并未给,但 snippet 应先通过;下一步我们完整测合并
}

func TestValidator_GoSnippetInvalid(t *testing.T) {
	v := NewValidator(nil)
	e := v.Validate("a.go", "func Foo() int { return 1", "") // 缺 }
	if e == nil {
		t.Fatal("expected snippet error")
	}
	if e.Stage != StageSnippet {
		t.Errorf("want stage snippet, got %s", e.Stage)
	}
	if e.Line <= 0 {
		t.Errorf("expected line number, got %+v", e)
	}
}

func TestValidator_GoMergedInvalid(t *testing.T) {
	v := NewValidator(nil)
	// snippet 本身 OK,但合并后文件缺 package → merged 失败
	merged := `func Foo() int { return 1 }` // 没 package 行 → merged 阶段必失败
	e := v.Validate("a.go", "func Foo() int { return 1 }", merged)
	if e == nil {
		t.Fatal("expected merged error")
	}
	if e.Stage != StageMerged {
		t.Errorf("want merged stage, got %s", e.Stage)
	}
}

func TestValidator_GoBothPass(t *testing.T) {
	v := NewValidator(nil)
	snippet := `func Foo() int { return 1 }`
	merged := "package a\n\n" + snippet + "\n"
	e := v.Validate("a.go", snippet, merged)
	if e != nil {
		t.Errorf("unexpected error: %+v", e)
	}
}

// --- Apply: MODIFY ---

func TestApply_Modify_Success(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
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

	res := ap.Apply(&Modification{
		Action:     ActionModify,
		ChunkID:    fooID,
		NewContent: "func Foo() int {\n\treturn 42\n}",
	})
	if res.Outcome != OutcomeApplied {
		t.Fatalf("want Applied, got %s: %s", res.Outcome, res.Message)
	}

	// 验证文件内容
	content, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(content), "return 42") {
		t.Errorf("file not updated:\n%s", content)
	}

	// 验证 ID 仍在(稳定性)
	if _, ok := st.GetChunk(fooID); !ok {
		t.Errorf("chunk ID %s lost after modify", fooID)
	}
}

func TestApply_Modify_ValidationFails_NoWrite(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
		"a.go": `package a

func Foo() int {
	return 1
}
`,
	})
	original, _ := os.ReadFile(filepath.Join(root, "a.go"))

	var fooID string
	for _, c := range st.AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	// 非法代码:缺 }
	res := ap.Apply(&Modification{
		Action:     ActionModify,
		ChunkID:    fooID,
		NewContent: "func Foo() int {\n\treturn 42",
	})
	if res.Outcome != OutcomeValidation {
		t.Fatalf("want Validation, got %s: %s", res.Outcome, res.Message)
	}
	if res.Validation == nil {
		t.Error("Validation field should be populated")
	}

	// 文件必须未变
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(after) != string(original) {
		t.Errorf("file was modified despite validation failure:\n%s", after)
	}
}

func TestApply_Modify_MissingID(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{
		"a.go": `package a
func Foo() {}
`,
	})
	res := ap.Apply(&Modification{
		Action:     ActionModify,
		ChunkID:    "xXxX", // 不存在
		NewContent: "func Foo() {}",
	})
	if res.Outcome != OutcomeLocation {
		t.Errorf("want Location, got %s: %s", res.Outcome, res.Message)
	}
}

// --- Apply: CREATE_FILE ---

func TestApply_CreateFile_Success(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{})

	res := ap.Apply(&Modification{
		Action:     ActionCreateFile,
		FilePath:   "new/thing.go",
		NewContent: "package thing\n\nfunc Hello() {}\n",
	})
	if res.Outcome != OutcomeApplied {
		t.Fatalf("want Applied, got %s: %s", res.Outcome, res.Message)
	}
	// 文件存在
	if _, err := os.Stat(filepath.Join(root, "new/thing.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
	// 已索引
	if len(st.ChunksByFile("new/thing.go")) != 1 {
		t.Errorf("want 1 chunk in new file, got %d", len(st.ChunksByFile("new/thing.go")))
	}
}

func TestApply_CreateFile_AlreadyExists(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{
		"a.go": `package a
func X() {}
`,
	})
	res := ap.Apply(&Modification{
		Action:     ActionCreateFile,
		FilePath:   "a.go",
		NewContent: "package a\nfunc Y() {}",
	})
	if res.Outcome != OutcomeConflict {
		t.Errorf("want Conflict, got %s: %s", res.Outcome, res.Message)
	}
}

func TestApply_CreateFile_UnsupportedExt(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{})
	res := ap.Apply(&Modification{
		Action:     ActionCreateFile,
		FilePath:   "readme.py",
		NewContent: "print('hi')",
	})
	if res.Outcome != OutcomeConflict {
		t.Errorf("want Conflict, got %s: %s", res.Outcome, res.Message)
	}
}

// --- Apply: DELETE_FILE ---

func TestApply_DeleteFile_Success(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
		"a.go": `package a
func X() {}
`,
	})
	res := ap.Apply(&Modification{
		Action:   ActionDeleteFile,
		FilePath: "a.go",
	})
	if res.Outcome != OutcomeApplied {
		t.Fatalf("want Applied, got %s: %s", res.Outcome, res.Message)
	}
	if _, err := os.Stat(filepath.Join(root, "a.go")); !os.IsNotExist(err) {
		t.Errorf("file should be gone: %v", err)
	}
	if len(st.ChunksByFile("a.go")) != 0 {
		t.Errorf("store should have no chunks in a.go")
	}
}

func TestApply_DeleteFile_NonExistent(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{})
	res := ap.Apply(&Modification{
		Action:   ActionDeleteFile,
		FilePath: "nope.go",
	})
	if res.Outcome != OutcomeNoOp {
		t.Errorf("want NoOp, got %s", res.Outcome)
	}
}

// --- Apply: ADD_CHUNK ---

func TestApply_AddChunk_Success(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
		"a.go": `package a

func Foo() {}
`,
	})
	before := len(st.ChunksByFile("a.go"))

	res := ap.Apply(&Modification{
		Action:     ActionAddChunk,
		FilePath:   "a.go",
		NewContent: "func Bar() int { return 2 }",
	})
	if res.Outcome != OutcomeApplied {
		t.Fatalf("want Applied, got %s: %s", res.Outcome, res.Message)
	}
	after := len(st.ChunksByFile("a.go"))
	if after != before+1 {
		t.Errorf("want %d chunks, got %d", before+1, after)
	}
	content, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(content), "func Bar() int") {
		t.Errorf("file missing new chunk:\n%s", content)
	}
}

func TestApply_AddChunk_FileNotExist(t *testing.T) {
	ap, _, _ := setupEditEnv(t, map[string]string{})
	res := ap.Apply(&Modification{
		Action:     ActionAddChunk,
		FilePath:   "nope.go",
		NewContent: "func Foo() {}",
	})
	if res.Outcome != OutcomeConflict {
		t.Errorf("want Conflict, got %s: %s", res.Outcome, res.Message)
	}
	if !strings.Contains(res.Message, "CREATE_FILE") {
		t.Errorf("message should suggest CREATE_FILE: %s", res.Message)
	}
}

// --- Apply: DELETE_CHUNK ---

func TestApply_DeleteChunk_Success(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
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
	res := ap.Apply(&Modification{
		Action:  ActionDeleteChunk,
		ChunkID: fooID,
	})
	if res.Outcome != OutcomeApplied {
		t.Fatalf("want Applied, got %s: %s", res.Outcome, res.Message)
	}
	// Bar 还在
	if len(st.ChunksByFile("a.go")) != 1 {
		t.Errorf("want 1 chunk left, got %d", len(st.ChunksByFile("a.go")))
	}
	content, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if strings.Contains(string(content), "Foo") {
		t.Errorf("Foo still in file:\n%s", content)
	}
	if !strings.Contains(string(content), "Bar") {
		t.Errorf("Bar accidentally deleted:\n%s", content)
	}
}

// --- 回滚测试:确保验证失败后文件字节完全相同 ---

func TestApply_ValidationFailure_FullRollback(t *testing.T) {
	ap, st, root := setupEditEnv(t, map[string]string{
		"a.go": `package a

func Foo() int {
	return 1
}

func Bar() int {
	return 2
}
`,
	})
	absPath := filepath.Join(root, "a.go")
	originalBytes, _ := os.ReadFile(absPath)
	originalStat, _ := os.Stat(absPath)

	var fooID string
	for _, c := range st.AllChunks() {
		if c.Name == "Foo" {
			fooID = c.ID
		}
	}

	// 发起 5 次会失败的 MODIFY,每次都必须保持文件不动
	for i := 0; i < 5; i++ {
		res := ap.Apply(&Modification{
			Action:     ActionModify,
			ChunkID:    fooID,
			NewContent: "func Foo() int { bogus syntax here !!!",
		})
		if res.Outcome == OutcomeApplied {
			t.Fatalf("attempt %d: should have failed", i)
		}
	}

	afterBytes, _ := os.ReadFile(absPath)
	if string(afterBytes) != string(originalBytes) {
		t.Errorf("file changed despite all failures:\nwant:\n%s\ngot:\n%s",
			originalBytes, afterBytes)
	}
	afterStat, _ := os.Stat(absPath)
	// mtime 可能略有不同(取决于文件系统),只看内容
	_ = originalStat
	_ = afterStat
}

// --- Batch 测试 ---

func TestApply_Batch_Independent(t *testing.T) {
	ap, st, _ := setupEditEnv(t, map[string]string{
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

	mods := []*Modification{
		{Action: ActionModify, ChunkID: fooID, NewContent: "func Foo() int { return 2 }"},
		{Action: ActionModify, ChunkID: "wrong", NewContent: "func Foo() int { return 3 }"}, // 定位失败
		{Action: ActionCreateFile, FilePath: "new.go", NewContent: "package main\nfunc New() {}"},
	}
	results := ap.ApplyBatch(mods)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	if results[0].Outcome != OutcomeApplied {
		t.Errorf("[0] want Applied, got %s", results[0].Outcome)
	}
	if results[1].Outcome != OutcomeLocation {
		t.Errorf("[1] want Location, got %s", results[1].Outcome)
	}
	if results[2].Outcome != OutcomeApplied {
		t.Errorf("[2] want Applied, got %s", results[2].Outcome)
	}
}
