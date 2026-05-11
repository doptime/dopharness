package skills

// AddRoute 在 router 里新增一条路由。
type AddRoute struct {
	Path        string   `json:"path" jsonschema:"description=路由 path,如 /api/foo"`
	HandlerName string   `json:"handler_name" jsonschema:"description=handler 函数名"`
	Method      string   `json:"method,omitempty" jsonschema:"description=HTTP 方法,默认 GET"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=可选的 tags,用于路由分组"`
}

const AddRouteSOP = `你正在执行"添加路由"任务。参数:
  Path: {{.Path}}
  Handler: {{.HandlerName}}
  Method: {{.Method}}
  Tags: {{.Tags}}

按以下步骤完成:
1. 用 search_chunks_by_name 找到 router 注册的 chunk
2. 用 read_chunk 看现有的注册模式
3. 用 add_chunk 添加 handler 函数
4. 用 modify_chunk 注册新路由

完成后用一句话回报结果。`

// RefactorFunction 把一个函数改名,并同步更新所有调用点。
type RefactorFunction struct {
	OldName string `json:"old_name" jsonschema:"description=原函数名"`
	NewName string `json:"new_name" jsonschema:"description=新函数名"`
}

const RefactorFunctionSOP = `你正在执行"函数改名"任务。参数:
  OldName: {{.OldName}}
  NewName: {{.NewName}}

步骤:
1. search_chunks_by_name {{.OldName}} 找到目标定义
2. read_chunk 确认是函数声明
3. modify_chunk 改函数名
4. 用 search_chunks_by_name 找所有引用,逐一 modify_chunk 改调用名
`

// HelperUnused 是一个没有对应 SOP 常量的辅助类型,加载时应该被跳过。
type HelperUnused struct {
	Foo string
}
