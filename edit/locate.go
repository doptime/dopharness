// Package edit 负责把 LLM 的修改建议安全地应用到磁盘。
//
// 这一层是 dopharness 对抗 LLM 幻觉的最后防线,设计上极度偏执:
//   - 所有定位错误(找不到/有歧义)都以结构化诊断返回,LLM 能自我纠正
//   - 所有写盘都是"先字节备份 → 语法校验 → 原子替换",失败必回滚
//   - 单个修改失败不影响批次中其他修改,结果独立汇总
package edit

import (
	"fmt"
	"strings"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// LocateOutcome 是定位结果的分类。
type LocateOutcome string

const (
	// LocateExact 按 ChunkID 精确命中。
	LocateExact LocateOutcome = "exact"
	// LocateFuzzyUnique 精确未命中,按 Name 回退后唯一匹配。
	// 调用方应当接受但记录警告(说明 LLM 报的 ID 不对)。
	LocateFuzzyUnique LocateOutcome = "fuzzy_unique"
	// LocateAmbiguous 按 Name 有多个候选。调用方必须拒绝,让 LLM 澄清。
	LocateAmbiguous LocateOutcome = "ambiguous"
	// LocateMissing 完全找不到。调用方应引导 LLM 检查是否该用 CREATE_FILE/ADD_CHUNK。
	LocateMissing LocateOutcome = "missing"
)

// LocateResult 是一次定位尝试的完整输出。
type LocateResult struct {
	Outcome    LocateOutcome
	Chunk      *chunk.Chunk   // LocateExact / LocateFuzzyUnique 时非 nil
	Candidates []*chunk.Chunk // LocateAmbiguous 时的候选集;其他情况可为 nil
	// Message 是给 LLM 看的人类可读解释。用于组装 tool 错误返回值。
	Message string
}

// Locator 封装在 ChunkStore 中定位一个 ChunkID 或 Name 的逻辑。
//
// 查找优先级:
//  1. 如果 targetID 非空,先按 ID 精确查
//  2. ID 未命中:如果 targetID 长相像 "file/path.go:Name" 的形式,提取尾部的 Name 做回退
//  3. 按原始 Name 或回退出的 Name 做 byName 查找
//  4. byName 命中 1 个 → FuzzyUnique;多个 → Ambiguous;0 个 → Missing
type Locator struct {
	Store store.ChunkStore
}

// NewLocator 构造 Locator。
func NewLocator(s store.ChunkStore) *Locator {
	return &Locator{Store: s}
}

// Locate 根据 LLM 提供的 targetID(可能是真 ID,也可能是 "path:name" 这种历史遗留)
// 尝试定位一个 chunk。如果 targetID 为空,会用 nameHint 兜底。
//
// 注意:LLM 很可能写错大小写或添加尾随空格。我们做最轻量的规范化(trim),
// 不做大小写改写 —— 代码符号对大小写敏感,宁可报错让 LLM 改也不要假装正确。
func (l *Locator) Locate(targetID string, nameHint string) *LocateResult {
	targetID = strings.TrimSpace(targetID)
	nameHint = strings.TrimSpace(nameHint)

	// 1. 精确 ID 命中
	if targetID != "" {
		if c, ok := l.Store.GetChunk(targetID); ok {
			return &LocateResult{
				Outcome: LocateExact,
				Chunk:   c,
				Message: "",
			}
		}
	}

	// 2. 从 targetID 里反推 Name
	//    场景:LLM 把遗留的 "pkg/foo.go:Bar" 当成 ID 用了
	fallbackName := nameHint
	if fallbackName == "" && targetID != "" {
		if idx := strings.LastIndex(targetID, ":"); idx >= 0 {
			fallbackName = strings.TrimSpace(targetID[idx+1:])
		} else {
			// 也可能直接就是个名字
			fallbackName = targetID
		}
	}

	if fallbackName == "" {
		return &LocateResult{
			Outcome: LocateMissing,
			Message: fmt.Sprintf("chunk not found: id %q has no matching chunk and no name hint provided", targetID),
		}
	}

	// 3. 按 Name 查
	candidates := l.Store.ChunksByName(fallbackName)
	switch len(candidates) {
	case 0:
		// 再尝试:如果 fallbackName 带点号(如 "User.Save"),看看是不是全限定
		// 此时 ChunksByName 应该已经处理;没命中就是真没有
		return &LocateResult{
			Outcome: LocateMissing,
			Message: fmt.Sprintf("chunk not found: no chunk with id %q or name %q. "+
				"If this should be a new function, use CREATE_FILE or ADD_CHUNK instead.",
				targetID, fallbackName),
		}
	case 1:
		msg := ""
		if targetID != "" && candidates[0].ID != targetID {
			msg = fmt.Sprintf("chunk id %q not found, but uniquely matched by name %q -> id %q. "+
				"Prefer using the correct id next time.",
				targetID, fallbackName, candidates[0].ID)
		}
		return &LocateResult{
			Outcome: LocateFuzzyUnique,
			Chunk:   candidates[0],
			Message: msg,
		}
	default:
		return &LocateResult{
			Outcome:    LocateAmbiguous,
			Candidates: candidates,
			Message: fmt.Sprintf("ambiguous chunk name %q: %d candidates. "+
				"Specify the full chunk id instead: %s",
				fallbackName, len(candidates), formatCandidates(candidates)),
		}
	}
}

// formatCandidates 把候选集格式化成 LLM 友好的字符串。
func formatCandidates(cs []*chunk.Chunk) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s (%s in %s)", c.ID, c.Kind, c.FilePath))
	}
	return strings.Join(parts, ", ")
}
