package skills

import (
	"bytes"
	"context"
	"fmt"
	"reflect"

	"github.com/doptime/llm"
)

// SubAgentRunner 是上层(harness)注入的能力:用 dopharness 的 built-in
// tool 集执行一段渲染好的 prompt。
//
// 参数:
//
//	ctx        —— 父 Agent 透传下来的 context(cancellation 链)
//	sopPrompt  —— 已经把 skill 的 struct 字段填到 SOP 模板后的最终 prompt
//	skillName  —— 触发本次执行的 skill 名,便于上层做 tracing / 防递归 / 限额
//
// 实现方负责:
//   - 起一个 sub-agent (典型用同一个 Harness 的 MainCaller 跑一次 agent.Call)
//   - 把 dopharness 的 built-in tools(modify_chunk / create_file / ...)注册给它
//   - **不要** 把 skill 集合也注册给 sub-agent(防递归;skill 的"展开"只发生一层)
//   - 把 sub-agent 的成败 / 工具执行摘要返回给 skill 调用方
//
// 返回 error 时,skill 这一次 toolcall 在父 Agent 视角是失败的,父级的 retry
// 机制会把错误信息塞回去再来一轮。
type SubAgentRunner func(ctx context.Context, sopPrompt string, skillName string) error

// AsLLMTool 把 Skill 包装成 llm.ToolInterface,可以直接传给 agent.UseTools。
//
// 工作流:
//  1. LLM 把 JSON 参数发给 skill 这个 toolcall
//  2. doptime/llm 的 DynamicTool.HandleCallback 把 JSON 反序列化进
//     reflect.New(s.StructType) 分配的 *T
//  3. sink 拿到 *T,用 text/template 渲染 SOPTemplate
//  4. sink 调 runner,把渲染好的 prompt 喂给 sub-agent
//  5. sub-agent 用 built-in tools 执行,完成后返回成败
//
// runner 通常由 harness 提供,skills 包不感知具体的 LLM / Agent 类型。
func (s *Skill) AsLLMTool(runner SubAgentRunner) llm.ToolInterface {
	if s == nil {
		return nil
	}
	if runner == nil {
		// 没有 runner 时退化为一个会报错的 sink,让上层在测试时能尽早发现配置漏配
		runner = func(ctx context.Context, sopPrompt string, skillName string) error {
			return fmt.Errorf("skill %q: SubAgentRunner 未配置", skillName)
		}
	}

	skillName := s.Name
	tmpl := s.SOPTemplate

	sink := func(value any, callMem map[string]any) error {
		if value == nil {
			return fmt.Errorf("skill %q: sink 收到 nil value", skillName)
		}
		// value 是 *T 形态;Elem() 拿到 T 给 text/template 用
		rv := reflect.ValueOf(value)
		if rv.Kind() != reflect.Ptr {
			return fmt.Errorf("skill %q: sink 期望 *T,实际收到 %T", skillName, value)
		}
		data := rv.Elem().Interface()

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("skill %q: 渲染 SOP 失败: %w", skillName, err)
		}

		// 从 callMem 抽 context;没有就 Background
		ctx := context.Background()
		if callMem != nil {
			if c, ok := callMem["Context"].(context.Context); ok && c != nil {
				ctx = c
			}
		}

		return runner(ctx, buf.String(), skillName)
	}

	return llm.NewToolFromType(s.Name, s.Description, s.StructType, sink)
}

// AsLLMTools 是 Registry 级别的便捷封装:把所有 skill 一次性转成 ToolInterface
// 切片,直接传给 agent.UseTools 即可。
func (r *Registry) AsLLMTools(runner SubAgentRunner) []llm.ToolInterface {
	skills := r.All()
	out := make([]llm.ToolInterface, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.AsLLMTool(runner))
	}
	return out
}
