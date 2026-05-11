// Package skills 实现 dopharness 的 "flywheel skill" 体系。
//
// 一个 skill 文件就是一个普通 Go 文件,内含:
//   - 一个或多个 type X struct {...} 声明 (定义参数 schema)
//   - 同名的 const XSOP = `...` 字符串常量 (描述执行步骤的 text/template)
//
// 框架启动时用 go/parser 读 skill 文件、用 reflect.StructOf 在运行时拼出
// struct 类型、用 llm.NewToolFromType 注册成 toolcall。LLM 调用 skill 时,
// 框架渲染 SOP 模板并把它喂给一个 sub-agent,sub-agent 用 dopharness 的
// built-in tool 集执行实际工作。
//
// 这个分层(policy vs mechanism)的核心好处:skill 是纯数据,加载/卸载/换版本
// 都不需要重启进程,Go GC 能正常回收旧版本;真正的"原子能力"由 dopharness
// 二进制提供,数量稀少、迭代慢,可以走传统的发版流程。
package skills

import (
	"fmt"
	"go/ast"
	"reflect"
	"strconv"
)

// anyType 是空接口的 reflect.Type。skill 字段允许使用 any/interface{}, 此时
// JSON 反序列化按"无类型"处理(数字 -> float64、对象 -> map[string]any)。
var anyType = reflect.TypeOf((*any)(nil)).Elem()

// astTypeToReflect 把一个 AST 类型表达式翻译成对应的 reflect.Type。
//
// MVP 范围内允许的形态:
//   - 内置基础类型:string / bool / int 各档位 / float32/64 / byte / rune / any
//   - slice / array (array 必须是 slice 写法,即 [N]T 不支持)
//   - map[K]V
//   - *T
//   - 匿名嵌套 struct
//   - 空 interface (interface{} == any)
//
// 不支持(故意拒绝):
//   - 限定类型 time.Time / pkg.Type —— 需要包级解析能力,MVP 不进
//   - 非空 interface —— LLM 没有合理诉求来写出它们
//   - 命名类型(typedef)—— 同上
//   - 嵌入字段 —— 字段必须显式起名,避免 reflect.StructOf 的边角行为
//
// 上述限制都通过明确的 error 报告给加载器,让 LLM 在重试时能改正。
func astTypeToReflect(expr ast.Expr) (reflect.Type, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		return basicIdentToReflect(t.Name)

	case *ast.ArrayType:
		if t.Len != nil {
			return nil, fmt.Errorf("固定长度数组不支持,请改用 slice ([]T)")
		}
		elem, err := astTypeToReflect(t.Elt)
		if err != nil {
			return nil, fmt.Errorf("slice 元素类型: %w", err)
		}
		return reflect.SliceOf(elem), nil

	case *ast.MapType:
		k, err := astTypeToReflect(t.Key)
		if err != nil {
			return nil, fmt.Errorf("map key 类型: %w", err)
		}
		v, err := astTypeToReflect(t.Value)
		if err != nil {
			return nil, fmt.Errorf("map value 类型: %w", err)
		}
		return reflect.MapOf(k, v), nil

	case *ast.StarExpr:
		elem, err := astTypeToReflect(t.X)
		if err != nil {
			return nil, fmt.Errorf("指针 elem 类型: %w", err)
		}
		return reflect.PointerTo(elem), nil

	case *ast.StructType:
		return BuildStructFromAST(t)

	case *ast.InterfaceType:
		if t.Methods != nil && len(t.Methods.List) > 0 {
			return nil, fmt.Errorf("非空 interface 不支持(skill 字段只能是数据)")
		}
		return anyType, nil

	case *ast.SelectorExpr:
		// time.Time / context.Context 这类。不支持是为了让 skill 自包含,
		// LLM 不需要去琢磨 dopharness 二进制里到底 import 了什么。
		return nil, fmt.Errorf("限定类型不支持(看到 %s.%s)。skill 字段只能用内置类型 + slice/map/pointer/嵌套 struct",
			exprIdentName(t.X), t.Sel.Name)

	case *ast.FuncType:
		return nil, fmt.Errorf("函数类型不能作为 skill 参数")

	case *ast.ChanType:
		return nil, fmt.Errorf("channel 类型不能作为 skill 参数")
	}
	return nil, fmt.Errorf("不支持的类型表达式: %T", expr)
}

// basicIdentToReflect 把内置标识符翻译为对应的 reflect.Type。
// 命名类型在这里被一概拒绝,避免 LLM 引用一个不存在于运行时的类型。
func basicIdentToReflect(name string) (reflect.Type, error) {
	switch name {
	case "string":
		return reflect.TypeOf(""), nil
	case "int":
		return reflect.TypeOf(int(0)), nil
	case "int8":
		return reflect.TypeOf(int8(0)), nil
	case "int16":
		return reflect.TypeOf(int16(0)), nil
	case "int32":
		return reflect.TypeOf(int32(0)), nil
	case "int64":
		return reflect.TypeOf(int64(0)), nil
	case "uint":
		return reflect.TypeOf(uint(0)), nil
	case "uint8", "byte":
		return reflect.TypeOf(uint8(0)), nil
	case "uint16":
		return reflect.TypeOf(uint16(0)), nil
	case "uint32":
		return reflect.TypeOf(uint32(0)), nil
	case "uint64":
		return reflect.TypeOf(uint64(0)), nil
	case "rune":
		// rune == int32 别名
		return reflect.TypeOf(int32(0)), nil
	case "float32":
		return reflect.TypeOf(float32(0)), nil
	case "float64":
		return reflect.TypeOf(float64(0)), nil
	case "bool":
		return reflect.TypeOf(false), nil
	case "any":
		return anyType, nil
	}
	return nil, fmt.Errorf("命名类型 %q 不支持(skill 字段只能用内置类型: string/int/float/bool/[]T/map/*T/struct/any)", name)
}

// BuildStructFromAST 把一个 AST 上的 struct 类型(命名或匿名都行)翻译成
// 对应的运行时 reflect.Type。
//
// 字段必须:
//   - 有显式名字(不允许嵌入)
//   - 名字以大写字母开头(可被 reflect / json 看到)
//   - 类型在 astTypeToReflect 的支持列表内
//
// tag 原样复制(包括 json / jsonschema / description 等),交给上层 schema 生成
// 与 json.Unmarshal 处理。
func BuildStructFromAST(st *ast.StructType) (reflect.Type, error) {
	if st.Fields == nil || len(st.Fields.List) == 0 {
		// 空 struct 在 reflect.StructOf 里是合法的(产生 struct{}),但作为 skill
		// 没有意义。允许它通过,让上层根据 SOP 判断是否需要参数。
		return reflect.StructOf(nil), nil
	}

	var fields []reflect.StructField
	for _, field := range st.Fields.List {
		ft, err := astTypeToReflect(field.Type)
		if err != nil {
			fname := "<embedded>"
			if len(field.Names) > 0 {
				fname = field.Names[0].Name
			}
			return nil, fmt.Errorf("字段 %s: %w", fname, err)
		}

		var tag reflect.StructTag
		if field.Tag != nil {
			// field.Tag.Value 形如 `json:"foo"` (含两端反引号),要先 unquote
			tagStr, qerr := strconv.Unquote(field.Tag.Value)
			if qerr != nil {
				return nil, fmt.Errorf("字段 %v 的 tag 解析失败: %w", field.Names, qerr)
			}
			tag = reflect.StructTag(tagStr)
		}

		if len(field.Names) == 0 {
			return nil, fmt.Errorf("不允许嵌入字段(每个字段必须显式起名)")
		}
		for _, name := range field.Names {
			if !ast.IsExported(name.Name) {
				return nil, fmt.Errorf("字段 %s 必须以大写字母开头(skill 字段必须可被外部读取)", name.Name)
			}
			fields = append(fields, reflect.StructField{
				Name: name.Name,
				Type: ft,
				Tag:  tag,
			})
		}
	}

	// reflect.StructOf 在字段名重复时 panic,我们在这里手动检查给出友好错误。
	seen := map[string]struct{}{}
	for _, f := range fields {
		if _, dup := seen[f.Name]; dup {
			return nil, fmt.Errorf("字段名 %s 重复", f.Name)
		}
		seen[f.Name] = struct{}{}
	}

	return reflect.StructOf(fields), nil
}

// exprIdentName 取一个表达式最外层的标识符名,仅用于错误信息。
// 比如对 time.Time,t.X 是 *ast.Ident{Name:"time"},这个函数返回 "time"。
func exprIdentName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}
