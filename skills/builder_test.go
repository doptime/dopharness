package skills

import (
	"go/ast"
	"go/parser"
	"reflect"
	"strings"
	"testing"
)

// 用 parser.ParseExpr 直接造类型表达式,测 astTypeToReflect 的覆盖。
func TestAstTypeToReflect_Supported(t *testing.T) {
	cases := []struct {
		expr     string
		wantKind reflect.Kind
		wantStr  string // reflect.Type.String() 的预期形态
	}{
		{"string", reflect.String, "string"},
		{"int", reflect.Int, "int"},
		{"int64", reflect.Int64, "int64"},
		{"uint8", reflect.Uint8, "uint8"},
		{"byte", reflect.Uint8, "uint8"},
		{"rune", reflect.Int32, "int32"},
		{"float64", reflect.Float64, "float64"},
		{"bool", reflect.Bool, "bool"},
		{"any", reflect.Interface, "interface {}"},
		{"[]string", reflect.Slice, "[]string"},
		{"[]int", reflect.Slice, "[]int"},
		{"[][]string", reflect.Slice, "[][]string"},
		{"map[string]int", reflect.Map, "map[string]int"},
		{"map[string][]int", reflect.Map, "map[string][]int"},
		{"*int", reflect.Ptr, "*int"},
		{"*string", reflect.Ptr, "*string"},
		{"interface{}", reflect.Interface, "interface {}"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.expr, err)
			}
			rt, err := astTypeToReflect(e)
			if err != nil {
				t.Fatalf("astTypeToReflect(%q): %v", tc.expr, err)
			}
			if rt.Kind() != tc.wantKind {
				t.Errorf("Kind = %v, want %v", rt.Kind(), tc.wantKind)
			}
			if rt.String() != tc.wantStr {
				t.Errorf("String() = %q, want %q", rt.String(), tc.wantStr)
			}
		})
	}
}

// 不支持的形态都应明确拒绝。
func TestAstTypeToReflect_Rejected(t *testing.T) {
	cases := []struct {
		expr       string
		wantSubstr string
	}{
		{"time.Time", "限定类型不支持"},
		{"context.Context", "限定类型不支持"},
		{"[3]int", "固定长度数组"},
		{"interface{ Read() error }", "非空 interface"},
		{"chan int", "channel"},
		{"func()", "函数类型"},
		{"FooBar", "命名类型"}, // 既不是内置类型,又没有 selector
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parse %q: %v (这本身可能合法或需要外层包装,跳过)", tc.expr, err)
			}
			_, err = astTypeToReflect(e)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// 测匿名嵌套 struct: BuildStructFromAST 是否能递归正确构造。
func TestBuildStructFromAST_NestedStruct(t *testing.T) {
	src := `package x
type S struct {
    Outer string
    Inner struct {
        A int
        B []string
    }
}`
	e, err := parser.ParseExpr("struct { A int; B []string }")
	if err != nil {
		t.Fatalf("parse expr: %v", err)
	}
	st, ok := e.(*ast.StructType)
	if !ok {
		t.Fatalf("expected StructType, got %T", e)
	}
	rt, err := BuildStructFromAST(st)
	if err != nil {
		t.Fatalf("BuildStructFromAST: %v", err)
	}
	if rt.NumField() != 2 {
		t.Errorf("NumField = %d, want 2", rt.NumField())
	}
	if rt.Field(0).Name != "A" || rt.Field(0).Type.Kind() != reflect.Int {
		t.Errorf("Field 0 = %v %v, want A int", rt.Field(0).Name, rt.Field(0).Type)
	}
	if rt.Field(1).Name != "B" || rt.Field(1).Type.Kind() != reflect.Slice {
		t.Errorf("Field 1 = %v %v, want B slice", rt.Field(1).Name, rt.Field(1).Type)
	}
	_ = src // src 仅用于读者参考
}

// 测嵌入字段被拒(MVP 限制)。
func TestBuildStructFromAST_RejectsEmbedded(t *testing.T) {
	e, err := parser.ParseExpr("struct { Foo; Bar string }")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := e.(*ast.StructType)
	_, err = BuildStructFromAST(st)
	if err == nil {
		t.Fatal("expected error for embedded field")
	}
}

// 测未导出字段被拒。
func TestBuildStructFromAST_RejectsUnexported(t *testing.T) {
	e, err := parser.ParseExpr("struct { foo string }")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := e.(*ast.StructType)
	_, err = BuildStructFromAST(st)
	if err == nil {
		t.Fatal("expected error for unexported field")
	}
}

// 测 tag 被正确保留(json/jsonschema 都要透传给后续 schema 生成)。
func TestBuildStructFromAST_TagsPreserved(t *testing.T) {
	e, err := parser.ParseExpr(`struct {
        Path string ` + "`json:\"path\" jsonschema:\"description=foo\"`" + `
    }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := e.(*ast.StructType)
	rt, err := BuildStructFromAST(st)
	if err != nil {
		t.Fatalf("BuildStructFromAST: %v", err)
	}
	f := rt.Field(0)
	if f.Tag.Get("json") != "path" {
		t.Errorf("json tag = %q, want %q", f.Tag.Get("json"), "path")
	}
	if f.Tag.Get("jsonschema") != "description=foo" {
		t.Errorf("jsonschema tag = %q, want %q", f.Tag.Get("jsonschema"), "description=foo")
	}
}

// 测重复字段名被拒。
func TestBuildStructFromAST_RejectsDuplicates(t *testing.T) {
	e, err := parser.ParseExpr(`struct {
        A string
        A int
    }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := e.(*ast.StructType)
	_, err = BuildStructFromAST(st)
	if err == nil {
		t.Fatal("expected error for duplicate field name")
	}
}
