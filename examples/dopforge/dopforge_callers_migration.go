// dopforge_callers_migration.go
//
// 本文件不是可编译的 Go 程序。它是一份精确的迁移指南,适用于两个 dopforge 用例:
//   1. main_english_cloze_svg_game.go
//   2. main_stock_trading_strategy.go
//
// 两个文件的 caller 样板代码几乎完全相同 (只有 triage prompt 文字略有不同),
// 所以迁移步骤一模一样。
//
// ============================================================================
// 总效果
// ============================================================================
//
// 每个文件:
//   - 删除 ~130 行 (callerSet + buildCallers + doptimeToolBuilder +
//     doptimeTriageCaller + doptimeMainCaller)
//   - 新增 ~10 行 (import llmadapter + cs := llmadapter.Default(genModel))
//   - 净减 ~120 行
//   - doptimeJudgeCaller 和 doptimeDistiller 保留不变 (它们是 dopforge 专有的)
//
// ============================================================================
// 步骤 1: 修改 import 块
// ============================================================================
//
// 在已有的 import 里加一行:
//
//   "github.com/doptime/dopharness/llmadapter"
//
// 完整 import 块变为 (以 english_cloze 为例):
//
//   import (
//       "context"
//       "encoding/json"
//       "flag"
//       "fmt"
//       "log"
//       "os"
//       "path/filepath"
//       "regexp"
//       "sort"
//       "strings"
//       "text/template"
//       "time"
//
//       df "github.com/doptime/dopforge"
//       "github.com/doptime/dopharness/gateway"        // 仍需: TriageDecisionPayload 等类型
//       "github.com/doptime/dopharness/llmadapter"     // ← 新增
//       "github.com/doptime/dopharness/tools"           // 仍需: tools.ToolBuilder 类型 (在 df.Config 里引用)
//       "github.com/doptime/llm"
//   )
//
// 注: gateway 和 tools 仍保留 import,因为 df.Config 的字段类型引用了它们。
// 但你不再需要自己 import 它们来写 caller 实现。
//
// ============================================================================
// 步骤 2: 删除以下函数 (两个文件里都有,几乎一字不差)
// ============================================================================
//
// DELETE: type callerSet struct { ... }
// DELETE: func buildCallers() *callerSet { ... }
// DELETE: func doptimeToolBuilder() tools.ToolBuilder { ... }
// DELETE: func doptimeTriageCaller(...) error { ... }
// DELETE: func doptimeMainCaller(...) error { ... }
//
// 合计约 130 行。
//
// 保留: doptimeJudgeCaller (dopforge 专有的 judge 调用)
// 保留: doptimeDistiller (dopforge 专有的 skill 蒸馏)
// 保留: doptimeMergeCaller (多阶段合并,dopforge 专有)
//
// ============================================================================
// 步骤 3: 在 main() 中替换 callers 构造
// ============================================================================
//
// 找到这一行:
//
//   callers := buildCallers()
//
// 替换为:
//
//   cs := llmadapter.Default(genModel)
//
// 然后找到 df.Config 的构造:
//
//   f, err := df.New(df.Config{
//       ...
//       TriageCaller:   callers.triage,        // ← 改
//       MainCaller:     callers.main,           // ← 改
//       ToolBuilder:    callers.toolBuilder,    // ← 改
//       ...
//   })
//
// 改为:
//
//   f, err := df.New(df.Config{
//       ...
//       TriageCaller:   cs.Triage,              // ← 新
//       MainCaller:     cs.Main,                // ← 新
//       ToolBuilder:    cs.ToolBuilder,          // ← 新
//       ...
//   })
//
// 其余字段 (Goal, SeedDir, WorkRoot, Pipeline, SharedMemory,
// SkillDistiller, Logger) 完全不变。
//
// ============================================================================
// 步骤 4: (可选) 用 MultiModel 分配不同角色模型
// ============================================================================
//
// 如果你想用便宜模型跑 triage、强模型跑 main:
//
//   cs := llmadapter.MultiModel(llmadapter.Models{
//       Main:   genModel,         // 用于生成编辑
//       Triage: llm.Qwen36_35ba3b,      // 用于 chunk 筛选 (便宜)
//   })
//
// Expand caller (Pass2) 也会自动 fallback 到 Triage 模型。
//
// ============================================================================
// 完整 diff (以 main_english_cloze_svg_game.go 为例)
// ============================================================================
//
// --- a/main_english_cloze_svg_game.go
// +++ b/main_english_cloze_svg_game.go
// @@ import
// + "github.com/doptime/dopharness/llmadapter"
//
// @@ func main() 内
// - callers := buildCallers()
// + cs := llmadapter.Default(genModel)
//
// @@ df.Config{} 内
// - TriageCaller:   callers.triage,
// - MainCaller:     callers.main,
// - ToolBuilder:    callers.toolBuilder,
// + TriageCaller:   cs.Triage,
// + MainCaller:     cs.Main,
// + ToolBuilder:    cs.ToolBuilder,
//
// @@ 删除以下函数 (~130 行)
// - type callerSet struct { ... }
// - func buildCallers() *callerSet { ... }
// - func doptimeToolBuilder() tools.ToolBuilder { ... }
// - func doptimeTriageCaller(...) { ... }
// - func doptimeMainCaller(...) { ... }
//
// ============================================================================
// 对 main_stock_trading_strategy.go 的 diff 完全相同。
// ============================================================================
//
// 两个文件的 doptimeJudgeCaller / repairJudgeJSON / tryBalanceBraces /
// logJudgeFailure 也是一字不差的副本。如果将来把它们提取到 llmadapter
// 作为 NewJudgeCaller(model, opts) 工厂函数,还能再减 ~100 行/文件。
// 但那是 dopforge 层的改进,不是 dopharness 层的,暂不做。
//
// ============================================================================
// 验证清单 (明天人工验证用)
// ============================================================================
//
// [ ] import 编译通过
// [ ] main() 里 cs := llmadapter.Default(genModel) 不报错
// [ ] df.Config 里 cs.Triage / cs.Main / cs.ToolBuilder 类型匹配
// [ ] 已删除的 5 个函数在文件里找不到
// [ ] doptimeJudgeCaller 仍在且未改动
// [ ] doptimeDistiller 仍在且未改动
// [ ] doptimeMergeCaller 仍在且未改动
// [ ] go build ./... 通过
// [ ] 实际运行 (单阶段或多阶段) 与 V2 行为一致

package _migration_guide_not_compilable
