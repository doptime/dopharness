// Package main — example showing the FINAL external usage pattern after
// the dopharness "complete Agent" refactor.
//
// 这个文件是 doc-only:它演示外部调用方在三刀改造之后用 dopharness
// 应该长什么样。作为对照,把改造前 (coffee_cloze_test.go) 几百行
// 的 wiring 压成了 ~40 行,且 LLM 已经能自主管理记忆。
//
//  改造前 (coffee_cloze_test.go V2):
//    - 12 个 SECTION,~1000 行
//    - 手写 audit-fix-audit 循环
//    - 手写 doptimeTriageCaller / doptimeMainCaller / doptimeToolBuilder
//    - 手写 prompt 模板拼接、retry 反馈追加、ANSI 颜色、verdict 解析
//    - 没有记忆累积:每次任务都是"白板"重新开始
//
//  改造后 (本文件):
//    - 一个 main 函数,~40 行
//    - 循环、prompt、retry、归档全部内化到 harness
//    - L4 自动归档,下次任务自动召回
//    - LLM 通过 task_reflect 写入自己的内省总结
//
// 这就是用户在第一次提问里要的"高度简化的接口,不需要关注记忆的细节"。

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/doptime/llm"

	"github.com/doptime/dopharness/harness"
	"github.com/doptime/dopharness/llmadapter"
)

func main() {
	// === 一行完成所有 wiring ===
	h, _, err := llmadapter.QuickStart(llmadapter.QuickConfig{
		ProjectRoot: "./your-project",
		Model:       llm.Qwen3627b, // 也可以用 Models{Main: ..., Triage: ...} 分配不同模型
		Logger:      func(e string, f map[string]any) { log.Printf("[%s] %v", e, f) },
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// === 首次使用前索引项目 (后续会自动增量) ===
	if _, err := h.Index(ctx); err != nil {
		log.Fatal(err)
	}

	// === 跑一个完整 Agent 任务 ===
	result, err := h.RunTask(ctx, harness.Task{
		Description: "把 ComputeScore 里 hint penalty 的 30% 改为可配置参数,默认 30%",

		// Verify 是唯一的"是否做完"裁定器,完全由调用方定义。
		// 这里假设有一个 runTests() 跑 go test 并返回失败用例。
		Verify: func(ctx context.Context) (harness.VerifyResult, error) {
			passed, output := runTests("./your-project")
			return harness.VerifyResult{
				Passed: passed,
				Diag:   output,
			}, nil
		},

		// 编辑完到 Verify 之间留点时间给编译器/HMR
		BetweenRounds: func(ctx context.Context, _ int) error {
			time.Sleep(2 * time.Second)
			return nil
		},

		MaxRounds:  3,
		ArchiveTag: "config-hint-penalty",
	})
	if err != nil {
		log.Fatalf("task crashed: %v", err)
	}

	// === 报告 ===
	fmt.Printf("Passed=%v Rounds=%d ArchiveID=%s\n",
		result.Passed, result.Rounds, result.ArchiveID)
	if len(result.Reflections) > 0 {
		fmt.Println("LLM reflections:")
		for _, r := range result.Reflections {
			fmt.Printf("  - %s\n", r)
		}
	}

	// 下次跑同样领域的任务,memory 自动召回最近 5 条 SessionRecord ——
	// 包括上面的 reflections —— 让 LLM 知道"上次怎么搞的"。
	// 调用方什么都不用做。
	if !result.Passed {
		os.Exit(1)
	}
}

// runTests 是占位:你的项目用什么测试框架,这里就调什么。
// 关键是:把"做完了没"的判断完全留给调用方,harness 只问 "Passed?"。
func runTests(projectRoot string) (passed bool, output string) {
	fmt.Printf("Running tests in %s...\n", projectRoot)
	// e.g. exec.Command("go", "test", "./...").CombinedOutput()
	// e.g. HTTP call to your audit backend
	// e.g. headless browser snapshot diff
	return true, ""
}
