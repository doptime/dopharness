// Package gateway 是 dopharness 的"上下文网关"—— 决定每个 chunk 以何种形态(忽略/摘要/原文)
// 进入最终发给 LLM 的 prompt。
//
// 核心流程:
//   Pass 1 (Triage):将所有 chunk 按 N 个一片并发送给便宜的 LLM,每片独立决策每个 chunk 的形态
//   Pass 2 (Expand):把 Pass1 选中为 SKELETON 的全部 chunk 整批送给 LLM,给 FULL 升级机会
//   Render:按三态分别渲染成 XML 包裹的字符串,供后续拼入最终 prompt
//
// 为什么做这一层:大项目有成千 chunk,如果全部以原文送入上下文,必然溢出 token。
// 直接全摘要又会让 LLM 失去足够细节去做精确修改。三态混合是"信息密度最大化"的工程答案。
package gateway

import (
	"github.com/doptime/dopharness/chunk"
)

// Mode 是单个 chunk 在最终 prompt 中的呈现形态。
type Mode string

const (
	// ModeIgnore:不出现在 prompt 里。只会作为一行计数("<ignored_chunks count=42/>")被提及。
	ModeIgnore Mode = "IGNORE"

	// ModeSkeleton:只提供签名 + "..."。LLM 能看到"有这么个东西",但不看实现。
	ModeSkeleton Mode = "SKELETON"

	// ModeFull:完整源码。适合即将被修改或被频繁引用的核心 chunk。
	ModeFull Mode = "FULL"
)

// Decision 是对一个 chunk 的单次决策。
// Reason 是给调试/日志看的,Agent 层可以忽略。
type Decision struct {
	ChunkID string
	Mode    Mode
	Reason  string
}

// DecisionMap 是一次 triage/expand 运行后所有 chunk 的最终裁定。
// Key 是 ChunkID。
type DecisionMap map[string]*Decision

// Merge 把另一张决策表合并进来。
// 规则:后来的决策只能"升级"Mode —— IGNORE < SKELETON < FULL,不能降级。
// 这是 Pass2 合并 Pass1 结果时的保护:Pass2 只能把 SKELETON 抬成 FULL,不能把 Pass1 已标 FULL 的砍回 SKELETON。
func (m DecisionMap) Merge(other DecisionMap) {
	for id, d := range other {
		cur, exists := m[id]
		if !exists {
			m[id] = d
			continue
		}
		if modeRank(d.Mode) > modeRank(cur.Mode) {
			m[id] = d
		}
	}
}

func modeRank(m Mode) int {
	switch m {
	case ModeFull:
		return 2
	case ModeSkeleton:
		return 1
	default:
		return 0 // IGNORE / 未知
	}
}

// ChunkView 是网关向 LLM 展示一个 chunk 的轻量视图。
// Pass1 中我们只送这种"一行签名 + 引用符号"的低 token 视图;
// Pass2 才会送完整 skeleton。
type ChunkView struct {
	ID       string
	Kind     chunk.Kind
	Name     string
	FilePath string
	// Signature 是一行签名(Skeleton 的第一行 /* 去掉 doc 后 */)
	Signature string
	// Refs 是这个 chunk 引用的外部符号名字列表(不含本身定义的符号)
	Refs []string
}

// ToView 把 chunk.Chunk 转成 Pass1 要用的 ChunkView。
func ToView(c *chunk.Chunk) *ChunkView {
	return &ChunkView{
		ID:        c.ID,
		Kind:      c.Kind,
		Name:      c.Name,
		FilePath:  c.FilePath,
		Signature: firstMeaningfulLine(c.Skeleton),
		Refs:      c.Refs,
	}
}

// firstMeaningfulLine 从 skeleton 里抽出一行"最有信息量的"签名文字。
// 策略:跳过以 "//" "/*" "*" 开头的注释行和空行,取第一条实代码行。
// 如果全是注释(罕见),退化为 skeleton 的第一行。
func firstMeaningfulLine(skeleton string) string {
	start := 0
	for i := 0; i < len(skeleton); i++ {
		if skeleton[i] == '\n' {
			line := skeleton[start:i]
			if isSignatureLine(line) {
				return trimLeadingSpace(line)
			}
			start = i + 1
		}
	}
	// 最后一行
	if start < len(skeleton) {
		line := skeleton[start:]
		if isSignatureLine(line) {
			return trimLeadingSpace(line)
		}
	}
	// 全是注释:返回首个非空行
	start = 0
	for i := 0; i < len(skeleton); i++ {
		if skeleton[i] == '\n' {
			line := skeleton[start:i]
			if trimLeadingSpace(line) != "" {
				return trimLeadingSpace(line)
			}
			start = i + 1
		}
	}
	if start < len(skeleton) {
		return trimLeadingSpace(skeleton[start:])
	}
	return ""
}

func isSignatureLine(line string) bool {
	t := trimLeadingSpace(line)
	if t == "" {
		return false
	}
	// 跳过注释行
	if len(t) >= 2 && (t[:2] == "//" || t[:2] == "/*") {
		return false
	}
	if t[0] == '*' {
		return false
	}
	return true
}

func trimLeadingSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
}
