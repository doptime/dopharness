package edit

import (
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/doptime/dopharness/chunk"
)

// ValidationStage 指示哪一级校验失败。
type ValidationStage string

const (
	// StageSnippet 是单片校验:仅检查 LLM 产出的新代码片段自身能否解析。
	// 对 Go 来说,我们会给片段包一个 "package _tmp\n" 让顶层声明能跑。
	StageSnippet ValidationStage = "snippet"
	// StageMerged 是合并校验:把片段拼回完整文件后再解析一次,确保上下文没破坏。
	StageMerged ValidationStage = "merged"
)

// ValidationError 是一个可供 LLM 自纠的结构化诊断。
//
// Message 例如:"expected '}', got 'func'",配上 Line/Column 后 LLM 通常一次就能改对。
type ValidationError struct {
	Stage   ValidationStage `json:"stage"`
	Line    int             `json:"line,omitempty"`
	Column  int             `json:"column,omitempty"`
	Message string          `json:"message"`
}

func (v *ValidationError) Error() string {
	if v.Line > 0 {
		return fmt.Sprintf("[%s] line %d col %d: %s", v.Stage, v.Line, v.Column, v.Message)
	}
	return fmt.Sprintf("[%s] %s", v.Stage, v.Message)
}

// Validator 提供 Go/TS 语法校验。TS 侧可以为 nil(没配 TSParser 时 TS 校验会短路放行)。
type Validator struct {
	// TSParser 如果非 nil,TS 校验会调用它的 Validate 方法。
	// 为了避免循环依赖,这里用接口抽象。
	TSParser TSValidator
}

// TSValidator 是 TS 校验的外部契约。
//
// 为避免 chunk→edit 的反向依赖,这里采用"只返回第一条错误的关键字段"的极简契约。
// edit 包不关心错误列表的底层类型,只要能拿到"有没有错 + 第一条错的行/列/信息"即可。
//
// chunk.TSParser 通过 TSValidatorFunc 适配(见 harness 层的组装)。
type TSValidator interface {
	// ValidateTS 接收代码和语言标签 ("ts"/"tsx"/"js"/"jsx"),
	// 返回 ok=是否通过、line/col/msg=第一条错误的定位(ok=true 时忽略)、err=校验器自身异常。
	ValidateTS(code, kind string) (ok bool, line, col int, msg string, err error)
}

// TSValidatorFunc 是一个便捷的函数适配器。
// 任何符合签名的普通函数都能当作 TSValidator 使用。
type TSValidatorFunc func(code, kind string) (ok bool, line, col int, msg string, err error)

func (f TSValidatorFunc) ValidateTS(code, kind string) (bool, int, int, string, error) {
	return f(code, kind)
}

// NewValidator 构造 Validator。tsV 可以为 nil。
func NewValidator(tsV TSValidator) *Validator {
	return &Validator{TSParser: tsV}
}

// Validate 执行两级校验。path 用于推导语言,snippet 是 LLM 新产出的代码片段,
// merged 是合并后的完整文件内容。
//
// 策略:
//   - snippet 通不过 → 直接 StageSnippet 错,LLM 只看它自己产出的片段就能定位
//   - snippet 通过,但 merged 不过 → StageMerged 错,说明上下文(import/大括号匹配)有问题
//   - 二者都通过 → nil
func (v *Validator) Validate(path string, snippet, merged string) *ValidationError {
	lang := detectLanguage(path)
	switch lang {
	case langGo:
		if e := v.validateGoSnippet(snippet); e != nil {
			return e
		}
		return v.validateGoMerged(path, merged)
	case langTS:
		if v.TSParser == nil {
			// 未配置 TS 校验器:放行,但这意味着错误代码可能写盘。
			// 在 Apply 层用合并后的 post-write 解析兜底。
			return nil
		}
		kind := tsScriptKind(path)
		if e := v.validateTSCode(snippet, kind, StageSnippet); e != nil {
			return e
		}
		return v.validateTSCode(merged, kind, StageMerged)
	default:
		// 未知语言:dopharness 只应该接到 Go/TS/JS/TSX/JSX 的修改请求。
		// 保险起见不阻断,返回 nil。调用方应在 Apply 层拒绝未知扩展。
		return nil
	}
}

type language int

const (
	langUnknown language = iota
	langGo
	langTS
)

func detectLanguage(path string) language {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return langGo
	case ".ts", ".tsx", ".js", ".jsx":
		return langTS
	}
	return langUnknown
}

func tsScriptKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tsx":
		return "tsx"
	case ".jsx":
		return "jsx"
	case ".js":
		return "js"
	}
	return "ts"
}

// validateGoSnippet 对单片 Go 代码做语法校验。
//
// 片段可能是:
//   - 完整的 func/type 顶层声明 → 直接 parse 即可
//   - 光秃秃的 "func" 开头且没有 package 行 → 包一层 "package _tmp\n"
//
// 检查:文件是否以 package 开头
func (v *Validator) validateGoSnippet(snippet string) *ValidationError {
	code := snippet
	trimmed := strings.TrimLeft(code, " \t\n\r")
	if !strings.HasPrefix(trimmed, "package ") {
		code = "package _tmp\n" + code
	}
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "snippet.go", code, parser.ParseComments)
	if err != nil {
		return goParseErrorToValidation(err, StageSnippet, len(code)-len(snippet))
	}
	return nil
}

// validateGoMerged 对合并后的完整 Go 文件做解析。
func (v *Validator) validateGoMerged(path, merged string) *ValidationError {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, path, merged, parser.ParseComments)
	if err != nil {
		return goParseErrorToValidation(err, StageMerged, 0)
	}
	return nil
}

// goParseErrorToValidation 把 go/parser.Error(或 scanner.ErrorList)转成结构化诊断。
//
// go/parser 的错误是 scanner.ErrorList,每项形如 "filename:line:col: msg"。
// 我们取第一条,并在 lineOffset > 0 时扣掉我们自己加的前缀行。
func goParseErrorToValidation(err error, stage ValidationStage, lineOffset int) *ValidationError {
	if err == nil {
		return nil
	}
	// scanner.ErrorList 实现了 error,我们按字符串解析
	msg := err.Error()
	// 典型格式: "snippet.go:3:10: expected '}', got 'func'"
	// 也可能是多行(ErrorList):取第一行
	firstLine := msg
	if nl := strings.IndexByte(msg, '\n'); nl >= 0 {
		firstLine = msg[:nl]
	}
	parts := strings.SplitN(firstLine, ":", 4)
	out := &ValidationError{Stage: stage, Message: firstLine}
	if len(parts) == 4 {
		var line, col int
		_, e1 := fmt.Sscanf(parts[1], "%d", &line)
		_, e2 := fmt.Sscanf(parts[2], "%d", &col)
		if e1 == nil && e2 == nil {
			// 减去我们自己加的 "package _tmp\n" 那一行
			if lineOffset > 0 {
				line-- // 我们只加了一行
				if line < 1 {
					line = 1
				}
			}
			out.Line = line
			out.Column = col
			out.Message = strings.TrimSpace(parts[3])
		}
	}
	return out
}

// validateTSCode 委托给 TSValidator 做 TS 校验。
func (v *Validator) validateTSCode(code, kind string, stage ValidationStage) *ValidationError {
	ok, line, col, msg, execErr := v.TSParser.ValidateTS(code, kind)
	if execErr != nil {
		return &ValidationError{
			Stage:   stage,
			Message: fmt.Sprintf("ts validator exec failed: %v", execErr),
		}
	}
	if ok {
		return nil
	}
	if msg == "" {
		msg = "ts validation failed with no details"
	}
	return &ValidationError{
		Stage:   stage,
		Line:    line,
		Column:  col,
		Message: msg,
	}
}

// 反向兜底:如果未来 chunk 包希望调用 edit 层的校验,暴露一个显式类型断言
var _ = chunk.Chunk{} // 保留 import
