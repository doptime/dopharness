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

按以下步骤完成。注意:context 里 <file path="..."> 段内的
[chunk xxxx FULL/SKELETON] 标记已经把项目中所有相关 chunk 列出来,
直接挑相关 chunk_id 操作即可。

1. 在 context 中定位 router 注册表 chunk(通常名字含 Router/Mux/Routes)
2. 用 add_chunk 在 handlers 文件末尾添加 handler 函数
3. 用 modify_chunk 把新路由注册到 router chunk

完成后用一句话回报结果。`

// RefactorFunction 把一个函数改名,并同步更新所有调用点。
type RefactorFunction struct {
	OldName string `json:"old_name" jsonschema:"description=原函数名"`
	NewName string `json:"new_name" jsonschema:"description=新函数名"`
}

const RefactorFunctionSOP = `你正在执行"函数改名"任务。参数:
  OldName: {{.OldName}}
  NewName: {{.NewName}}

步骤(全部 chunk 都在 context 里 [chunk xxxx FULL/SKELETON] 标记中可见):

1. 找到 {{.OldName}} 的定义,记下它的 chunk_id
2. 扫描所有 Refs 包含 {{.OldName}} 的 chunk(即调用点)
3. modify_chunk 改定义本身,把名字改成 {{.NewName}}
4. 对每个调用点 modify_chunk 把调用改成 {{.NewName}}
`

// HelperUnused 是一个没有对应 SOP 常量的辅助类型,加载时应该被跳过。
type HelperUnused struct {
	Foo string
}
