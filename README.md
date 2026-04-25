# dopharness

一个 Go 语言的 **AST 级上下文网关**,让 LLM 在修改代码仓库时产生的幻觉最小化。

思路来自 [lsdefine/GenericAgent](https://github.com/lsdefine/GenericAgent) 的分层记忆理念,
实现上深度对接 [doptime/llm](https://github.com/doptime/llm) 的工具调用系统。

## 它解决什么问题

直接把整个代码库塞给 LLM 修改有两个老大难:

1. **Token 爆炸**:大项目上千个函数,全量原文送入必然溢出上下文
2. **修改幻觉**:LLM 看到半截代码容易"脑补",结果改对了一半、漏掉一半、位置错位

dopharness 通过三步把这两件事都处理了:

1. **AST 切片**:源码按函数/类型切成 chunk,每个 chunk 有稳定的 4 字符 ID
2. **三态上下文网关**:请求前先让便宜的小模型判断每个 chunk 应该
   - `FULL` 原文完整展示(将要被修改的)
   - `SKELETON` 只给签名(可能被引用的)
   - `IGNORE` 完全不出现(无关的,只报总数)
3. **外科修改**:LLM 只能通过 ToolCall 发 `modify_chunk(id, new_content)`,
   服务端做两级语法校验,失败回滚

## 安装

```bash
go get github.com/doptime/dopharness
```

TypeScript/JavaScript 支持需要 [Bun](https://bun.sh):

```bash
curl -fsSL https://bun.sh/install | bash
```

## 快速开始

```go
package main

import (
    "context"
    "fmt"
    "log"
    "text/template"

    "github.com/doptime/dopharness/gateway"
    "github.com/doptime/dopharness/harness"
    "github.com/doptime/dopharness/tools"
    "github.com/doptime/llm"
)

func main() {
    // 1. 把 doptime/llm 的 NewTool 适配为 dopharness 能用的 ToolBuilder
    toolBuilder := tools.ToolBuilderFunc(func(name, desc string, handler any) any {
        // llm.NewTool 接受强类型回调,我们统一用 any 过一道
        switch h := handler.(type) {
        case func(*tools.ModifyChunkPayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.DeleteChunkPayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.AddChunkPayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.CreateFilePayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.DeleteFilePayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.ReadChunkPayload):
            return llm.NewTool(name, desc, h)
        case func(*tools.SearchChunksByNamePayload):
            return llm.NewTool(name, desc, h)
        }
        panic(fmt.Sprintf("unknown handler type: %T", handler))
    })

    // 2. 定义 Triage Agent (Pass1):用小模型快速判断每个 chunk 的形态
    triageCaller := func(p gateway.TriagePromptParams, sink func(*gateway.TriageDecisionPayload)) error {
        tpl := template.Must(template.New("triage").Parse(`
你是代码上下文筛选助手。用户任务:
{{.UserPrompt}}

这是一些候选 chunk(格式:id | kind | name | signature | refs):
{{range .Views}}{{.ID}} | {{.Kind}} | {{.Name}} | {{.Signature}} | {{.Refs}}
{{end}}

对每个 chunk,通过 TriageDecision 工具提交你的判断。
- 如果 chunk 就是要被修改的,标 FULL
- 如果 chunk 被将要修改的代码引用(参数类型、调用关系),标 SKELETON
- 其余标 IGNORE
`))

        var payload *gateway.TriageDecisionPayload
        tool := llm.NewTool("TriageDecision", "提交 chunk 裁定", func(p *gateway.TriageDecisionPayload) {
            payload = p
        })
        agent := llm.NewAgent(tpl, tool).UseModels(llm.ModelDefault)
        if err := agent.Call(map[string]any{
            "UserPrompt": p.UserPrompt,
            "Views":      p.Views,
        }); err != nil {
            return err
        }
        sink(payload)
        return nil
    }

    // 3. Main Agent:真正做修改的那一次
    mainCaller := func(systemPrompt, userPrompt string, toolList []any) error {
        tpl := template.Must(template.New("main").Parse(`{{.UserPrompt}}`))

        // toolList 是 []any,我们需要展开并断言到 llm.ToolInterface
        llmTools := make([]llm.ToolInterface, 0, len(toolList))
        for _, t := range toolList {
            llmTools = append(llmTools, t.(llm.ToolInterface))
        }

        agent := llm.NewAgent(tpl).UseModels(llm.ModelDefault)
        // 把每个工具注册进去
        for _, t := range llmTools {
            agent.UseTools(t)
        }
        // doptime/llm 的 Agent 只支持 user prompt,把 system 当前缀拼上
        return agent.Call(map[string]any{
            "UserPrompt": systemPrompt + "\n\n" + userPrompt,
        })
    }

    // 4. 构造 Harness
    h, err := harness.New(harness.Config{
        ProjectRoot:  "./my-repo",
        TriageCaller: triageCaller,
        MainCaller:   mainCaller,
        EnableExpand: false, // Pass2 可选,先关着
        MaxRetries:   3,
    })
    if err != nil {
        log.Fatal(err)
    }

    // 5. 索引
    if _, err := h.Index(context.Background()); err != nil {
        log.Fatal(err)
    }

    // 6. 注册工具(Run 之前必须做)
    h.AsLLMTools(toolBuilder)

    // 7. 跑!
    report, err := h.Run(context.Background(), "给 extractGoFunc 函数加上 context.Context 参数")
    if err != nil {
        log.Fatal(err)
    }
    if report.Success {
        fmt.Printf("✅ 成功 (轮数=%d)\n", report.Rounds)
    } else {
        fmt.Printf("❌ 失败 (轮数=%d)\n", report.Rounds)
        for i, round := range report.ToolRecords {
            for _, rec := range round {
                if !rec.OK() {
                    fmt.Printf("  轮 %d %s: %s\n", i+1, rec.ToolName, rec.Result.Message)
                }
            }
        }
    }
}
```

## 包结构

```
dopharness/
├── chunk/     AST 切片:chunk.go + parser_go.go + parser_ts.go + sidecar.ts
├── store/     chunk 持久化:JSONStore(默认)+ ChunkStore 接口
├── index/     目录扫描 + 增量解析调度
├── edit/      修改回写:Modification、Applier、两级语法校验、文件锁
├── gateway/   三态上下文网关:Pass1 并发分片 + Pass2 整批升级 + XML 渲染
├── memory/    GenericAgent 风格的 L0-L4 分层记忆
├── tools/     5+2 个 llm.Tool 封装:modify_chunk / create_file / ... / read_chunk
└── harness/   顶层门面:New / Index / BuildContext / Run / AsLLMTools
```

## 记忆目录约定

Harness 默认会在 `<ProjectRoot>/.dopharness/memory/` 下寻找:

```
memory/
├── rules.md       # L0:系统级戒律(可选,没有时用 DefaultRulesFallback)
├── facts.md       # L2:项目级事实("本项目用 Go 1.22、gin、sqlx")
├── skills/        # L3:可复用 SOP,每个 .md 文件一个
│   ├── add_route.md
│   └── run_tests.md
└── sessions/      # L4:历史会话摘要,由 memory.SessionRecordsLayer.Append 写入
```

## 设计决策记录

| 决策 | 选择 |
|---|---|
| Chunk ID 形式 | 3 字节随机 + base64url = 4 字符。与文件名/符号解耦,永不变 |
| 增量索引判据 | mtime 一级过滤 + xxhash 二级过滤(对抗 `git checkout`) |
| TS 分析方式 | Bun sidecar 子进程 + 批量模式(首次冷启动后一次解析所有文件) |
| 上下文判定 | 两 Pass LLM(无向量无图),Pass1 分片并发,Pass2 整批升级 |
| 修改失败策略 | 两级语法校验 → 写盘 → reindex → 任一步失败字节级回滚 |
| Run 的重试 | 最多 3 轮;失败时把"上一轮工具摘要"追加到 user prompt |
| 存储层 | 默认 JSON 文件(chunks.json + files.json);接口可替换 |

## 测试

```bash
# Go 测试(不需要 Bun)
go test ./chunk ./store ./index ./edit ./gateway ./memory ./tools ./harness -run '!TS'

# 全量(需要 Bun 或 DOPHARNESS_BUN=<shim>)
DOPHARNESS_BUN=/path/to/bun go test ./...
```

## 许可

见项目仓库。
