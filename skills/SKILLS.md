
# dopharness/skills

dopharness 的 **flywheel skill** 体系。LLM 在做项目过程中会发现重复出现的"步骤套路"(比如"加一条路由"涉及四步固定的 chunk 操作)。skill 就是把这些套路固化成一段**纯数据**:参数 schema(struct)+ 步骤模板(SOP),不写任何 callback 代码。

## 一个 skill 长什么样

skill 是一个普通 Go 文件 (注:不进 `go build`,在运行时由 `go/parser` 读)。每个 skill 由一对配套声明组成:

```go
package skills  // 包名只是为了过 parser,不会被加载

// AddRoute 在 router 里新增一条路由。
type AddRoute struct {
    Path        string   `json:"path" jsonschema:"description=路由 path,如 /api/foo"`
    HandlerName string   `json:"handler_name" jsonschema:"description=handler 函数名"`
    Method      string   `json:"method,omitempty" jsonschema:"description=HTTP 方法,默认 GET"`
    Tags        []string `json:"tags,omitempty"`
}

const AddRouteSOP = `你正在执行"添加路由"任务。参数:
  Path: {{.Path}}
  Handler: {{.HandlerName}}
  Method: {{.Method}}

按以下步骤完成。context 里 <file path="..."> 段内的 [chunk xxxx FULL/SKELETON]
标记已经把项目中所有相关 chunk 列出来了,直接挑相关的 chunk_id 操作即可。

1. 在 context 中定位 router 注册表 chunk(通常名字含 Router/Mux/Routes)
2. 用 add_chunk 在 handlers 文件末尾添加 handler 函数
3. 用 modify_chunk 把新路由注册进 router chunk
`
```

四条规则:

1. **type 名字 + `SOP` 后缀 = 常量名**,这是配对依据。`type AddRoute` ↔ `const AddRouteSOP`。
2. **type 上方的 doc 注释成为 LLM 看到的 description**。开头如果是类型名(`AddRoute ...`),framework 会自动 strip 掉。
3. **常量必须是字符串字面量**,raw 或普通都行,不支持表达式。
4. **没有匹配 SOP 常量的 type 会被忽略**,允许文件里写辅助类型。

## 字段类型支持范围

| 形态                    | 支持 | 备注                          |
| ----------------------- | ---- | ----------------------------- |
| `string`/`int*`/`uint*` | ✅   | 全档位                        |
| `float32`/`float64`     | ✅   |                               |
| `bool`                  | ✅   |                               |
| `byte`/`rune`           | ✅   | uint8 / int32 别名            |
| `any` / `interface{}`   | ✅   | JSON 反序列化按"无类型"处理   |
| `[]T`                   | ✅   | T 必须是支持范围内的类型      |
| `map[K]V`               | ✅   | 同上                          |
| `*T`                    | ✅   | 同上                          |
| 嵌套匿名 struct         | ✅   | 字段同样要求导出              |
| 限定类型 `time.Time`    | ❌   | 加载时报错                    |
| 命名类型 `MyAlias`      | ❌   | 加载时报错                    |
| 固定数组 `[3]int`       | ❌   | 改用 slice                    |
| 非空 interface          | ❌   | skill 字段必须是数据          |
| 嵌入字段                | ❌   | 字段必须显式起名              |
| 未导出字段              | ❌   | 必须以大写字母开头            |

设计意图:让 skill 文件**自包含**,LLM 不需要琢磨 dopharness 二进制里到底 import 了什么。

## SOP 模板

模板用 Go 标准库的 `text/template`,数据上下文是填充好的 struct 值。

- `{{.Path}}` 读字段
- `{{if .Tags}}...{{end}}` 条件
- `{{range .Tags}}{{.}} {{end}}` 遍历

模板**在加载时就解析**;有语法错误会直接让 `LoadSkillFile` 失败,不会等到 LLM 调用时才暴露。

## 怎么挂进 dopharness

skills 包不直接知道 LLM 怎么调、tools 怎么注册 —— 它通过一个 `SubAgentRunner` 函数注入这些能力。Harness 这边大致这样接:

```go
import (
    "context"
    "github.com/doptime/dopharness/skills"
    "github.com/doptime/llm"
)

// 1. 启动时加载 skills
registry := skills.NewRegistry()
if err := registry.LoadDir("./.dopharness/skills"); err != nil {
    log.Fatal(err)
}

// 2. 定义 sub-agent runner —— harness 自己负责"怎么用 built-in tools 跑一段 prompt"
runner := func(ctx context.Context, sopPrompt, skillName string) error {
    sysPrompt := "You are a sub-agent executing a planned procedure. " +
        "Use the available tools to complete each step."
    // 关键: built-in tools only,**不要** 把 skill 也传给 sub-agent(防递归)
    return harness.MainCaller(ctx, sysPrompt, sopPrompt, harness.BuiltinTools)
}

// 3. 把 skill 转成 ToolInterface,和 built-in tools 一起注册给主 agent
agent := llm.NewAgent(...)
agent.UseTools(harness.BuiltinTools...)
agent.UseTools(registry.AsLLMTools(runner)...)
```

主 agent 看到的 tool 集 = built-in + skills,LLM 自由选择。它选 skill 时,framework 在内部展开成"渲染 SOP + 起 sub-agent",sub-agent 看到的 tool 集只有 built-in。**展开只发生一层**,这是防递归的核心。

## 版本升级 / 热加载

`Registry.Add(s)` 是按名字覆盖的,所以更新一个 skill 文件后,重新调一次 `registry.LoadDir(...)` 就能生效。旧版本失去引用后被 GC 回收 —— 这是这套架构相比 Go plugin 的关键优势(plugin 包加载后无法 unload)。

注:`reflect.StructOf` 产出的 type 在 runtime 内部有一份 KB 级的元数据缓存,长期看是缓慢增长的。对开发者交互会话(几小时一关)这个量级完全可以忽略。

## Skill 是只读的 vs 可写的

当前 MVP:framework **只读** skill 目录。LLM 不能直接通过 toolcall 创建 skill —— 它只能通过 dopharness 的 built-in tools(`create_file` / `add_chunk`)写到 skill 目录,然后由开发者手动 reload 或重启。

Phase 2 会加 `register_skill` / `unregister_skill` 这一对原子 toolcall,让飞轮自闭环。但要先看 LLM 实际写出来的 skill 质量再决定。

## 调试

每个 `*Skill` 上的 `SOPSource` 字段保留了模板原文,失败时 log 一下能直观看到 LLM 生成了什么模板。`SourcePath` 指向源文件位置。
