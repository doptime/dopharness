# dopharness flywheel skills — Phase 1 交付说明

本次交付包含两个项目的代码改动,共同实现 dopharness 的 "flywheel skills" 体系。

## 文件清单

```
llm/                              # 增量改动到 github.com/doptime/llm
├── tool_dynamic.go               # 新增:NewToolFromType / DynamicTool
└── tool_dynamic_test.go          # 新增:对应测试

dopharness/skills/                # 新增包到 github.com/doptime/dopharness/skills
├── SKILLS.md                     # 用户文档
├── skill.go                      # Skill 类型 + Registry
├── builder.go                    # AST 类型表达式 → reflect.Type
├── loader.go                     # .go 文件 → []*Skill
├── bridge.go                     # Skill → llm.ToolInterface
├── builder_test.go
├── loader_test.go
├── bridge_test.go
└── testdata/
    └── valid_skill.go            # 测试夹具(两个 demo skill)
```

## 架构 in 一段话

**Built-in tools** 是 dopharness 二进制里的 Go 函数(`modify_chunk` 等 7 个),进程内 + 闭包 callback,迭代慢,新增需要重启进程。**Flywheel skills** 是文件系统上的 .go 文件,纯数据(struct shape + 文本模板),没有 callback,加载/卸载/换版本不动 Go 代码段。LLM 在主 agent 上下文同时看到 built-in 和 skill 两类工具;选了 skill 后 framework 渲染 SOP 起 sub-agent,sub-agent 用 built-in tools 干活。

这是 policy/mechanism 分层(Lampson 1976),工业上极成熟:Claude Skills、Unity prefab、Photoshop action、K8s CRD、Semantic Kernel plugin/planner 都是这个骨架。

## 两包关系

doptime/llm 这边的改动**最小切口**:加一个 `NewToolFromType` 把"运行时 reflect.Type 注册为 toolcall"这个能力公开,不动 `tool.go`,不改 `Tool[v]` 的语义。schema 生成路径(`getFieldName` / `buildSchemaForType`)直接复用,行为与 `NewTool[v]` 完全一致。

dopharness/skills 这边是**纯新增包**,通过一个 `SubAgentRunner` 接口把"如何跑 sub-agent"这件事抽象出去,自己不依赖 dopharness 的 harness 内部细节。Harness 那边只需要写 ~10 行接线代码(见 SKILLS.md 末尾的示例)。

## 验证方式

沙箱无 Go 工具链,本次未跑 `go test`。但代码:

- 严格对齐 `Tool[v].HandleCallback` 的现有语义(JSON unmarshal + mapstructure decode + 字段反向落 CallMemory),所以行为差异只在"类型来源"
- `astTypeToReflect` 的覆盖路径都有对应测试用例,正反两面覆盖
- `bridge_test.go` 模拟了从 LLM 提交 JSON 到 sub-agent 收到 prompt 的完整链路

落地后建议跑序: `go vet ./... && go test ./skills/... -run . -v`。

## Phase 2 预告

本次刻意没做的事:

1. **`register_skill` / `unregister_skill` toolcall** —— 让 LLM 能自己创建/淘汰 skill。要先看 Phase 1 跑起来后 LLM 实际写出来的 skill 质量再设计这一层(否则容易过早乐观)。

2. **`add_builtin_tool` + 重启机制** —— 让 LLM 能写新的 built-in 原子能力。架构上倾向: 写 .go 文件 → `go vet` → `go build` → 产出 next-binary → LLM 主动调 `restart_dopharness` 用 `syscall.Exec` 替换当前进程。**重启交给 LLM 决定时机**,而不是 framework 自动触发,因为 LLM 比 framework 更清楚当前任务能不能中断。

3. **跨语言 skill 格式** —— 当前是 .go 文件,纯 syntax 上是 Go 但语义上其实只用了 type 声明 + 字符串常量两种结构。如果未来要让非 Go 项目也能用,可以加一个 markdown frontmatter 格式作为平行入口。但这是真有需求时再做。

第 1 项门槛最低,一个会话就能跑通;第 2 项是真正让飞轮自我演化的关键,但工程量也大。建议先跑 Phase 1 至少几十次会话,看实际有多少 skill 是"built-in 已能组合出来"的、多少需要新原子能力,再决定 Phase 2 的优先级。

## 应用 patch

```bash
# 1. doptime/llm
cd $GOPATH/src/github.com/doptime/llm
cp /path/to/this/llm/tool_dynamic.go .
cp /path/to/this/llm/tool_dynamic_test.go .
go test ./...

# 2. dopharness
cd $GOPATH/src/github.com/doptime/dopharness
mkdir -p skills/testdata
cp /path/to/this/dopharness/skills/*.go skills/
cp /path/to/this/dopharness/skills/*.md skills/
cp /path/to/this/dopharness/skills/testdata/*.go skills/testdata/
go test ./skills/...
```
