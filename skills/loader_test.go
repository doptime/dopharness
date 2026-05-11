package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 加载 testdata/valid_skill.go 并验证抽出来的 skill 数量与字段。
func TestLoadSkillFile_ValidFixture(t *testing.T) {
	path := filepath.Join("testdata", "valid_skill.go")
	skills, err := LoadSkillFile(path)
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	// AddRoute + RefactorFunction = 2 个;HelperUnused 没 SOP 应该被忽略
	if len(skills) != 2 {
		t.Fatalf("got %d skills, want 2; names=%v", len(skills), skillNames(skills))
	}

	byName := map[string]*Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}

	add, ok := byName["AddRoute"]
	if !ok {
		t.Fatal("AddRoute not found")
	}
	if add.Description == "" {
		t.Error("AddRoute description should be non-empty")
	}
	if !strings.Contains(add.Description, "router") {
		t.Errorf("AddRoute description = %q, expected to contain 'router'", add.Description)
	}
	// description 应该 strip 掉了开头的 "AddRoute"
	if strings.HasPrefix(add.Description, "AddRoute") {
		t.Errorf("description still has type name prefix: %q", add.Description)
	}
	// 字段检查
	if add.StructType.NumField() != 4 {
		t.Errorf("AddRoute has %d fields, want 4", add.StructType.NumField())
	}
	pathField, ok := add.StructType.FieldByName("Path")
	if !ok {
		t.Fatal("Path field not found")
	}
	if pathField.Tag.Get("json") != "path" {
		t.Errorf("Path json tag = %q, want path", pathField.Tag.Get("json"))
	}
	tagsField, ok := add.StructType.FieldByName("Tags")
	if !ok {
		t.Fatal("Tags field not found")
	}
	if tagsField.Type.Kind() != reflect.Slice || tagsField.Type.Elem().Kind() != reflect.String {
		t.Errorf("Tags field type = %v, want []string", tagsField.Type)
	}
	// SOP 模板可执行
	if add.SOPTemplate == nil {
		t.Fatal("SOPTemplate is nil")
	}

	refactor, ok := byName["RefactorFunction"]
	if !ok {
		t.Fatal("RefactorFunction not found")
	}
	if refactor.StructType.NumField() != 2 {
		t.Errorf("RefactorFunction has %d fields, want 2", refactor.StructType.NumField())
	}

	// HelperUnused 不应进入 skill 列表
	if _, ok := byName["HelperUnused"]; ok {
		t.Error("HelperUnused 不应该被加载(它没有匹配的 SOP 常量)")
	}
}

// 端到端: 加载文件 → 通过 Registry → 渲染 SOP。
func TestLoadAndRender(t *testing.T) {
	path := filepath.Join("testdata", "valid_skill.go")
	skills, err := LoadSkillFile(path)
	if err != nil {
		t.Fatalf("LoadSkillFile: %v", err)
	}
	r := NewRegistry()
	for _, s := range skills {
		r.Add(s)
	}

	add, ok := r.Get("AddRoute")
	if !ok {
		t.Fatal("AddRoute not in registry")
	}

	// 用 reflect.New 分配一个实例,填上字段,用模板渲染
	v := reflect.New(add.StructType).Elem()
	v.FieldByName("Path").SetString("/api/foo")
	v.FieldByName("HandlerName").SetString("fooHandler")
	v.FieldByName("Method").SetString("GET")
	v.FieldByName("Tags").Set(reflect.ValueOf([]string{"api", "v1"}))

	var sb strings.Builder
	if err := add.SOPTemplate.Execute(&sb, v.Interface()); err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := sb.String()
	if !strings.Contains(rendered, "/api/foo") {
		t.Errorf("rendered missing Path:\n%s", rendered)
	}
	if !strings.Contains(rendered, "fooHandler") {
		t.Errorf("rendered missing HandlerName:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[api v1]") {
		t.Errorf("rendered missing Tags:\n%s", rendered)
	}
}

// 内联坏文件: 包含 time.Time 应该被 builder 拒绝。
func TestLoadSkillFile_RejectsQualifiedType(t *testing.T) {
	src := `package skills

// Bad 用了限定类型 time.Time,应该被拒绝。
type Bad struct {
	When time.Time ` + "`json:\"when\"`" + `
}

const BadSOP = "do something at {{.When}}"
`
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadSkillFile(p)
	if err == nil {
		t.Fatal("expected error for time.Time field, got nil")
	}
	if !strings.Contains(err.Error(), "限定类型") {
		t.Errorf("error = %q, want containing '限定类型'", err.Error())
	}
}

// 内联文件: 模板语法错误应该在加载时被发现。
func TestLoadSkillFile_RejectsBadTemplate(t *testing.T) {
	src := `package skills

// Bad 模板里有未闭合的动作。
type Bad struct {
	X string ` + "`json:\"x\"`" + `
}

const BadSOP = "{{.X 没闭合"
`
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadSkillFile(p)
	if err == nil {
		t.Fatal("expected template parse error, got nil")
	}
	if !strings.Contains(err.Error(), "SOP 模板") && !strings.Contains(err.Error(), "template") {
		t.Errorf("error = %q, want containing template error", err.Error())
	}
}

// Registry.LoadDir 走完整目录加载。
func TestRegistry_LoadDir(t *testing.T) {
	r := NewRegistry()
	if err := r.LoadDir("testdata"); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	got := r.All()
	if len(got) != 2 {
		t.Errorf("got %d skills, want 2; names=%v", len(got), skillNames(got))
	}
}

// LoadDir 对不存在的目录返回 nil(空注册表是合法状态)。
func TestRegistry_LoadDir_Missing(t *testing.T) {
	r := NewRegistry()
	if err := r.LoadDir(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("LoadDir for missing dir should be nil error, got %v", err)
	}
	if len(r.All()) != 0 {
		t.Error("registry should be empty after loading missing dir")
	}
}

func skillNames(skills []*Skill) []string {
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.Name
	}
	return out
}
