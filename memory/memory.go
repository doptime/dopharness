// Package memory 实现 GenericAgent 风格的分层记忆 (L0-L4)。
//
// 设计哲学:
//   - 每一层都是"一组文本片段 + 一条渲染策略",不引入复杂的持久化
//   - 所有层共享同一个接口 Layer,聚合时按优先级顺序写进 system / user prompt
//   - L1 (项目索引) 不在这里实现 —— gateway 包已经做了,memory 只管把 gateway 产出的
//     字符串放到正确的位置
//
// 输出约定:
//   - system:进入 LLM 的 system message,适合"永久有效的规则"
//   - user:跟随用户 prompt 一起的前置内容,适合"情境相关"的事实/技能/历史
package memory

import "time"

// RenderCtx 携带一次渲染所需的上下文。
// gateway 产出的三态 prompt 通过 ContextFromGateway 注入,memory 不关心它是怎么算出来的。
type RenderCtx struct {
	// UserPrompt 是用户原始任务描述。用于 L3 skill 选择器等场景。
	UserPrompt string

	// ContextFromGateway 是 gateway.BuildContext 返回的字符串(三态 chunk XML)。
	// memory 会把它嵌在 L1 的位置输出。
	ContextFromGateway string

	// Now 是当前时间。默认为调用时的 time.Now(),测试里可注入固定值。
	Now time.Time
}

// Layer 是单个记忆层的抽象。
//
// 实现应当:
//   - 通过 Prefix 返回一个人类可读的节名(用于调试 / 日志)
//   - Render 返回两段字符串:system 段和 user 段,任一可为空
//   - 线程安全:多个 goroutine 可能同时调用 Render(例如并发请求)
type Layer interface {
	Prefix() string
	Render(ctx *RenderCtx) (system string, user string, err error)
}

// Memory 是所有 Layer 的聚合体,对外暴露统一 Render。
//
// 输出顺序按层级固定:L0 -> L2 -> L1(gateway 上下文) -> L3 -> L4。
// 这个顺序的理由:
//   - L0 是最稳定的规则,放 system 最前
//   - L2 是项目级事实,也偏 system 性质
//   - L1 是动态选出的代码上下文,放 user 里靠前位置,让 LLM 马上看到
//   - L3 是可选技能,放代码上下文之后
//   - L4 是历史,最末尾
type Memory struct {
	L0 Layer // Meta Rules   -> system
	L1 Layer // Insight Index -> user (依赖 ContextFromGateway)
	L2 Layer // Global Facts -> system
	L3 Layer // Task Skills  -> user
	L4 Layer // Session Records -> user
}

// Render 组合所有层的输出,返回最终的 system / user 两段。
//
// 每一层的失败都会被捕获为 non-fatal —— 除非 L0/L2(核心规则)失败,否则其他层失败时
// 用空字符串替代并把错误汇总返回。调用方可以根据需要决定是否严格处理。
func (m *Memory) Render(ctx *RenderCtx) (system, user string, errs []error) {
	if ctx == nil {
		ctx = &RenderCtx{}
	}
	if ctx.Now.IsZero() {
		ctx.Now = time.Now()
	}

	var sys, usr []string

	// 严格:L0 失败算致命
	if m.L0 != nil {
		s, u, err := m.L0.Render(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		appendNonEmpty(&sys, s)
		appendNonEmpty(&usr, u)
	}

	// 严格:L2 失败算致命
	if m.L2 != nil {
		s, u, err := m.L2.Render(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		appendNonEmpty(&sys, s)
		appendNonEmpty(&usr, u)
	}

	// 宽松:L1 失败时仍然发 prompt,但会返回错误供调用方感知
	if m.L1 != nil {
		s, u, err := m.L1.Render(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		appendNonEmpty(&sys, s)
		appendNonEmpty(&usr, u)
	}

	if m.L3 != nil {
		s, u, err := m.L3.Render(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		appendNonEmpty(&sys, s)
		appendNonEmpty(&usr, u)
	}

	if m.L4 != nil {
		s, u, err := m.L4.Render(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		appendNonEmpty(&sys, s)
		appendNonEmpty(&usr, u)
	}

	return joinSections(sys), joinSections(usr), errs
}

// appendNonEmpty 把非空字符串追加进 out。
func appendNonEmpty(out *[]string, s string) {
	if s != "" {
		*out = append(*out, s)
	}
}

// joinSections 用两个换行分隔各段,保持可读性。
func joinSections(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	total := 0
	for _, p := range parts {
		total += len(p) + 2
	}
	buf := make([]byte, 0, total)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, '\n', '\n')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}
