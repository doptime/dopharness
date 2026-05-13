
// Package tools 把 dopharness 的修改能力封装成可注册到 LLM Agent 的工具。
//
// 每个 Action 都是一个独立的 Tool —— 尽管底层都走 edit.Applier,
// 分开暴露的好处是:
//   - 每个工具的 JSON Schema 最小化,LLM 看到的参数列表短而清晰
//   - LLM 在选择工具时不用先决定 Action 再填字段,而是直接挑对工具
//   - 工具名本身传达了意图,更容易被 LLM 正确使用
//
// 本包不直接依赖 github.com/doptime/llm —— Tool 定义通过 Factory 函数返回
// (函数签名里接收已经由 llm 包提供的 NewTool 构造器),实现解耦。
package tools

import (
	"fmt"

	"github.com/doptime/dopharness/edit"
)

// Record 是工具执行结果的对外汇报记录。
// Runner 在 Agent Loop 里收集所有 Record,决定是否需要进入重试轮。
type Record struct {
	ToolName string         // 被调用的工具名 (modify_chunk / create_file / ...)
	Request  any            // LLM 提交的原始参数(Payload struct 的值)
	Result   *edit.ApplyResult
}

// OK 返回本次调用是否成功。非成功意味着进入下一轮重试时需要把错误信息喂回 LLM。
func (r *Record) OK() bool {
	if r.Result == nil {
		return false
	}
	switch r.Result.Outcome {
	case edit.OutcomeApplied, edit.OutcomeNoOp:
		return true
	}
	return false
}

// Collector 是工具执行记录的汇聚器。
// 每个 Tool 的回调闭包持有它的指针,执行完就 Push 一条 Record 进去。
//
// Runner 每轮开始前会 Reset,轮结束后读取 Records 决定下一步。
type Collector struct {
	records []*Record
}

// NewCollector 构造。
func NewCollector() *Collector { return &Collector{} }

// Push 追加一条记录。
func (c *Collector) Push(r *Record) { c.records = append(c.records, r) }

// Records 返回当前所有记录。
func (c *Collector) Records() []*Record { return c.records }

// Reset 清空。Runner 在重试轮之间调用。
func (c *Collector) Reset() { c.records = nil }

// AllOK 当且仅当收集到至少一条记录,且全部 OK 时返回 true。
// 用于判断本轮 Agent 是否"有所作为"且"没有失败"。
func (c *Collector) AllOK() bool {
	if len(c.records) == 0 {
		return false
	}
	for _, r := range c.records {
		if !r.OK() {
			return false
		}
	}
	return true
}

// AnyFailure 返回是否有任何一条记录未成功。
func (c *Collector) AnyFailure() bool {
	for _, r := range c.records {
		if !r.OK() {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------
// 工具 Payload —— 每种 Action 一个专属 struct。
//
// 字段 tag 遵循 github.com/doptime/llm 的 jsonschema 风格,LLM 将基于
// 这些描述决定如何填参。描述里对"新手 LLM"友好,尽量具体。
// ----------------------------------------------------------------

// ModifyChunkPayload 是 MODIFY 工具的参数。
type ModifyChunkPayload struct {
	ChunkID string `json:"chunk_id" jsonschema:"description=目标 chunk 的 4 字符 ID。必须来自 <project_context> 中出现过的 ID"`
	// NewContent 要求是完整的顶层声明:
	//   - Go: 完整的 func/method/type 声明(含 doc 注释)
	//   - TS/JS: 完整的 function/class/interface 声明
	// 不能只给局部代码片段。
	NewContent string `json:"new_content" jsonschema:"description=替换后的完整顶层声明(含签名和主体);必须是合法语法"`
}

// DeleteChunkPayload 是 DELETE_CHUNK 工具的参数。
type DeleteChunkPayload struct {
	ChunkID string `json:"chunk_id" jsonschema:"description=要删除的 chunk 的 4 字符 ID"`
}

// AddChunkPayload 是 ADD_CHUNK 工具的参数。
type AddChunkPayload struct {
	FilePath   string `json:"file_path" jsonschema:"description=目标文件的相对路径(正斜杠,如 pkg/foo/bar.go);文件必须已存在"`
	NewContent string `json:"new_content" jsonschema:"description=要追加的完整顶层声明"`
}

// CreateFilePayload 是 CREATE_FILE 工具的参数。
type CreateFilePayload struct {
	FilePath   string `json:"file_path" jsonschema:"description=新文件的相对路径(正斜杠);扩展名必须是 .go/.ts/.tsx/.js/.jsx;文件不能已存在"`
	NewContent string `json:"new_content" jsonschema:"description=文件的完整初始内容(Go 必须包含 package 行)"`
}

// DeleteFilePayload 是 DELETE_FILE 工具的参数。
type DeleteFilePayload struct {
	FilePath string `json:"file_path" jsonschema:"description=要删除的文件的相对路径"`
}

// ----------------------------------------------------------------
// 工具定义 —— 通过 LLMToolFactory 接口适配到 doptime/llm
// ----------------------------------------------------------------

// ToolBuilder 是一个抽象的工具构造函数,对应 doptime/llm 的 llm.NewTool 签名
// (name string, description string, handler func(*T)) → ToolInterface。
//
// 这个包不 import llm,由 harness 层在组装时传入 llm.NewTool 作为 ToolBuilder。
//
// 返回 any 是因为不同 T 会产生不同静态类型的工具;harness 会接 []any,再由
// llm.Agent.UseTools 消费(它接 ...ToolInterface 的 variadic,any 可以类型断言回去)。
type ToolBuilder interface {
	Build(name, description string, handler any) any
}

// ToolBuilderFunc 是 ToolBuilder 的函数式适配器。
type ToolBuilderFunc func(name, description string, handler any) any

func (f ToolBuilderFunc) Build(name, description string, handler any) any {
	return f(name, description, handler)
}

// Bundle 汇总一组已注册的工具与它们共用的 Collector。
type Bundle struct {
	Tools     []any      // 可直接展开为 llm.Agent.UseTools(...) 的参数
	Collector *Collector // 所有工具写入同一个收集器
}

// BuildEditingTools 用 ToolBuilder 构造 5 个修改类工具。
//
// 不再提供 read_chunk / search_chunks_by_name —— Renderer 已经按文件分组把
// 相关 chunk 全量(或骨架)写进 prompt,LLM 没必要单独再问。
//
// 返回的 Bundle.Tools 顺序:
//   [0] modify_chunk
//   [1] delete_chunk
//   [2] add_chunk
//   [3] create_file
//   [4] delete_file
func BuildEditingTools(ap *edit.Applier, b ToolBuilder) *Bundle {
	bundle := &Bundle{
		Collector: NewCollector(),
	}

	// -- modify_chunk --
	modifyHandler := func(p *ModifyChunkPayload) {
		mod := &edit.Modification{
			Action:     edit.ActionModify,
			ChunkID:    p.ChunkID,
			NewContent: p.NewContent,
		}
		res := ap.Apply(mod)
		bundle.Collector.Push(&Record{
			ToolName: "modify_chunk",
			Request:  p,
			Result:   res,
		})
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"modify_chunk",
		"替换一个已存在 chunk 的完整内容。用于修改函数/方法/类型等顶层声明。必须通过 chunk_id 定位;new_content 必须是完整的顶层声明。",
		modifyHandler,
	))

	// -- delete_chunk --
	deleteChunkHandler := func(p *DeleteChunkPayload) {
		res := ap.Apply(&edit.Modification{
			Action:  edit.ActionDeleteChunk,
			ChunkID: p.ChunkID,
		})
		bundle.Collector.Push(&Record{
			ToolName: "delete_chunk",
			Request:  p,
			Result:   res,
		})
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"delete_chunk",
		"删除一个已存在的 chunk(从所在文件中移除该顶层声明)。文件其他 chunk 不受影响。",
		deleteChunkHandler,
	))

	// -- add_chunk --
	addChunkHandler := func(p *AddChunkPayload) {
		res := ap.Apply(&edit.Modification{
			Action:     edit.ActionAddChunk,
			FilePath:   p.FilePath,
			NewContent: p.NewContent,
		})
		bundle.Collector.Push(&Record{
			ToolName: "add_chunk",
			Request:  p,
			Result:   res,
		})
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"add_chunk",
		"往一个已存在的文件末尾追加一个新的顶层声明。用于在现有文件里新增函数/方法/类型。若文件不存在应改用 create_file。",
		addChunkHandler,
	))

	// -- create_file --
	createFileHandler := func(p *CreateFilePayload) {
		res := ap.Apply(&edit.Modification{
			Action:     edit.ActionCreateFile,
			FilePath:   p.FilePath,
			NewContent: p.NewContent,
		})
		bundle.Collector.Push(&Record{
			ToolName: "create_file",
			Request:  p,
			Result:   res,
		})
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"create_file",
		"创建一个新文件并写入完整内容。文件不能已存在;扩展名必须是受支持的语言(.go/.ts/.tsx/.js/.jsx)。",
		createFileHandler,
	))

	// -- delete_file --
	deleteFileHandler := func(p *DeleteFilePayload) {
		res := ap.Apply(&edit.Modification{
			Action:   edit.ActionDeleteFile,
			FilePath: p.FilePath,
		})
		bundle.Collector.Push(&Record{
			ToolName: "delete_file",
			Request:  p,
			Result:   res,
		})
	}
	bundle.Tools = append(bundle.Tools, b.Build(
		"delete_file",
		"删除整个文件及其所有 chunk。谨慎使用。",
		deleteFileHandler,
	))

	return bundle
}

// BuildSummary 返回所有 Record 的紧凑人类可读总结,
// 用于把"上一轮发生了什么"塞给 LLM 做自纠。
func (c *Collector) BuildSummary() string {
	if len(c.records) == 0 {
		return "no tool was called"
	}
	out := ""
	for i, r := range c.records {
		prefix := "OK"
		if !r.OK() {
			prefix = "FAILED"
		}
		out += fmt.Sprintf("[%d] %s %s: outcome=%s; message=%s\n",
			i+1, prefix, r.ToolName, r.Result.Outcome, r.Result.Message)
	}
	return out
}
