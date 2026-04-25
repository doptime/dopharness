package gateway

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// --- 辅助构造 ---

// mkChunk 造一个 chunk,ID 由调用方指定(便于断言)。
func mkChunk(id, name, path string, kind chunk.Kind, body string) *chunk.Chunk {
	return &chunk.Chunk{
		ID:       id,
		Name:     name,
		FilePath: path,
		Kind:     kind,
		Skeleton: fmt.Sprintf("func %s() { /* ... */ }", name),
		Body:     body,
	}
}

// populateStore 把一组 chunk 塞进内存 store(走 UpsertFile 会重分配 ID,这里不合适;
// 所以我们直接用一个自定义的 stub store)。
type stubStore struct {
	chunks map[string]*chunk.Chunk
	files  map[string]*store.FileMeta
	mu     sync.RWMutex
}

func newStubStore(chunks ...*chunk.Chunk) *stubStore {
	s := &stubStore{
		chunks: map[string]*chunk.Chunk{},
		files:  map[string]*store.FileMeta{},
	}
	for _, c := range chunks {
		s.chunks[c.ID] = c
	}
	return s
}

func (s *stubStore) Load() error  { return nil }
func (s *stubStore) Flush() error { return nil }
func (s *stubStore) GetChunk(id string) (*chunk.Chunk, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.chunks[id]
	return c, ok
}
func (s *stubStore) AllChunks() []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*chunk.Chunk, 0, len(s.chunks))
	for _, c := range s.chunks {
		out = append(out, c)
	}
	return out
}
func (s *stubStore) ChunksByName(name string) []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*chunk.Chunk
	for _, c := range s.chunks {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}
func (s *stubStore) ChunksByFile(path string) []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*chunk.Chunk
	for _, c := range s.chunks {
		if c.FilePath == path {
			out = append(out, c)
		}
	}
	return out
}
func (s *stubStore) GetFileMeta(p string) (*store.FileMeta, bool) { m, ok := s.files[p]; return m, ok }
func (s *stubStore) AllFiles() []*store.FileMeta {
	out := make([]*store.FileMeta, 0, len(s.files))
	for _, m := range s.files {
		out = append(out, m)
	}
	return out
}
func (s *stubStore) UpsertFile(path, hash string, mtime int64, newChunks []*chunk.Chunk) ([]*chunk.Chunk, error) {
	return newChunks, nil
}
func (s *stubStore) DeleteFile(path string) error { return nil }

var _ store.ChunkStore = (*stubStore)(nil)

// --- Pass 1 基础测试 ---

// 一个"理想"的 TriageCaller 模拟器:根据 UserPrompt 里的关键词决定 chunk 的模式。
// 规则(两轮决策,模拟 LLM 会综合考虑依赖关系):
//  1. chunk.Name 出现在 prompt 中 → FULL
//  2. 否则,chunk.Name 出现在"被标 FULL 的 chunk 的 Refs 里" → SKELETON
//  3. 其余 → IGNORE
//
// 注意:这里一个分片内 LLM 能看到所有 views 的 Refs,所以模拟器也看到,这与真实 LLM 行为接近。
func namingTriageCaller(t *testing.T) TriageCaller {
	return func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error {
		t.Helper()
		prompt := strings.ToLower(params.UserPrompt)

		// 第一轮:确定哪些是 FULL,同时收集它们的 refs
		fullIDs := map[string]bool{}
		refSet := map[string]bool{} // 被 FULL chunk 引用的符号名
		for _, v := range params.Views {
			if strings.Contains(prompt, strings.ToLower(v.Name)) {
				fullIDs[v.ID] = true
				for _, r := range v.Refs {
					refSet[strings.ToLower(r)] = true
				}
			}
		}
		// 第二轮:给每个 chunk 发决策
		out := &TriageDecisionPayload{}
		for _, v := range params.Views {
			mode := "IGNORE"
			if fullIDs[v.ID] {
				mode = "FULL"
			} else if refSet[strings.ToLower(v.Name)] {
				mode = "SKELETON"
			}
			out.Decisions = append(out.Decisions, TriageItem{
				ChunkID: v.ID,
				Mode:    mode,
				Reason:  "mock",
			})
		}
		sink(out)
		return nil
	}
}

func TestSelect_BasicTriage(t *testing.T) {
	// 场景:用户要改 Foo。Foo 调用了 Bar。应当 Foo=FULL, Bar=SKELETON, Baz=IGNORE
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "Foo", FilePath: "a.go", Kind: chunk.KindFunction,
			Skeleton: "func Foo()", Body: "func Foo() { Bar() }",
			Refs: []string{"Bar"}},
		{ID: "bbbb", Name: "Bar", FilePath: "a.go", Kind: chunk.KindFunction,
			Skeleton: "func Bar()", Body: "func Bar() {}"},
		{ID: "cccc", Name: "Baz", FilePath: "b.go", Kind: chunk.KindFunction,
			Skeleton: "func Baz()", Body: "func Baz() {}"},
	}

	sel, err := NewSelector(SelectorConfig{
		ShardSize:   10,
		Concurrency: 2,
		Triage:      namingTriageCaller(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	decisions, report := sel.Select("please modify Foo to log", chunks)

	if decisions["aaaa"].Mode != ModeFull {
		t.Errorf("Foo: want FULL, got %s", decisions["aaaa"].Mode)
	}
	if decisions["bbbb"].Mode != ModeSkeleton {
		t.Errorf("Bar: want SKELETON, got %s (reason: %s)", decisions["bbbb"].Mode, decisions["bbbb"].Reason)
	}
	if decisions["cccc"].Mode != ModeIgnore {
		t.Errorf("Baz: want IGNORE, got %s", decisions["cccc"].Mode)
	}
	if report.FullCount != 1 || report.SkeletonCount != 1 || report.IgnoreCount != 1 {
		t.Errorf("counts off: %+v", report)
	}
}

func TestSelect_SharingWhenMany(t *testing.T) {
	// 200 个 chunk,分片应当 > 1 且并发
	chunks := make([]*chunk.Chunk, 200)
	for i := 0; i < 200; i++ {
		chunks[i] = &chunk.Chunk{
			ID:       fmt.Sprintf("c%03d", i),
			Name:     fmt.Sprintf("Fn%d", i),
			FilePath: "big.go",
			Kind:     chunk.KindFunction,
			Skeleton: "func ...",
		}
	}
	var shardsSeen int32
	caller := func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error {
		atomic.AddInt32(&shardsSeen, 1)
		out := &TriageDecisionPayload{}
		for _, v := range params.Views {
			out.Decisions = append(out.Decisions, TriageItem{ChunkID: v.ID, Mode: "IGNORE"})
		}
		sink(out)
		return nil
	}
	sel, _ := NewSelector(SelectorConfig{
		ShardSize:   50,
		Concurrency: 4,
		Triage:      caller,
	})

	decisions, report := sel.Select("x", chunks)
	if len(decisions) != 200 {
		t.Errorf("decisions want 200, got %d", len(decisions))
	}
	if report.ShardsCount != 4 {
		t.Errorf("shards want 4, got %d", report.ShardsCount)
	}
	if atomic.LoadInt32(&shardsSeen) != 4 {
		t.Errorf("caller called want 4, got %d", shardsSeen)
	}
}

func TestSelect_MissingCoverageDefaultsIgnore(t *testing.T) {
	// LLM 漏掉了一个 chunk:gateway 必须补为 IGNORE
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A"},
		{ID: "bbbb", Name: "B"},
	}
	caller := func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error {
		// 故意只决策 aaaa
		sink(&TriageDecisionPayload{Decisions: []TriageItem{
			{ChunkID: "aaaa", Mode: "FULL"},
		}})
		return nil
	}
	sel, _ := NewSelector(SelectorConfig{Triage: caller})

	decisions, report := sel.Select("x", chunks)
	if decisions["bbbb"].Mode != ModeIgnore {
		t.Errorf("missing coverage should default to IGNORE, got %s", decisions["bbbb"].Mode)
	}
	if len(report.MissingCoverage) != 1 {
		t.Errorf("missing coverage list want 1, got %v", report.MissingCoverage)
	}
}

func TestSelect_ShardFailureIsolated(t *testing.T) {
	// 1 个分片失败,其他分片结果仍然生效
	chunks := make([]*chunk.Chunk, 100)
	for i := 0; i < 100; i++ {
		chunks[i] = &chunk.Chunk{ID: fmt.Sprintf("c%03d", i), Name: fmt.Sprintf("F%d", i)}
	}
	var callCount int32
	caller := func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error {
		idx := atomic.AddInt32(&callCount, 1)
		if idx == 1 {
			return fmt.Errorf("simulated LLM outage")
		}
		out := &TriageDecisionPayload{}
		for _, v := range params.Views {
			out.Decisions = append(out.Decisions, TriageItem{ChunkID: v.ID, Mode: "SKELETON"})
		}
		sink(out)
		return nil
	}
	sel, _ := NewSelector(SelectorConfig{
		ShardSize: 50, Concurrency: 1, // 顺序执行,保证 idx==1 是第一片
		Triage: caller,
	})
	decisions, report := sel.Select("x", chunks)

	if len(report.TriageErrors) != 1 {
		t.Errorf("want 1 triage error, got %d", len(report.TriageErrors))
	}
	// 失败分片里的 chunk 默认 IGNORE
	// 成功分片里的 chunk 应 SKELETON
	// 至少能看到 SKELETON 和 IGNORE 各若干
	if report.SkeletonCount == 0 {
		t.Errorf("second shard should produce SKELETONs")
	}
	if report.IgnoreCount == 0 {
		t.Errorf("failed shard chunks should default to IGNORE")
	}
	// decisions 覆盖所有 100
	if len(decisions) != 100 {
		t.Errorf("decisions want 100, got %d", len(decisions))
	}
}

// --- Pass 2 测试 ---

func TestSelect_ExpandUpgradesSkeletonToFull(t *testing.T) {
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A", Skeleton: "func A()"},
		{ID: "bbbb", Name: "B", Skeleton: "func B()"},
	}
	// Pass1:A=SKELETON, B=IGNORE
	triage := func(params TriagePromptParams, sink func(*TriageDecisionPayload)) error {
		sink(&TriageDecisionPayload{Decisions: []TriageItem{
			{ChunkID: "aaaa", Mode: "SKELETON"},
			{ChunkID: "bbbb", Mode: "IGNORE"},
		}})
		return nil
	}
	// Pass2:升级 A 到 FULL
	expand := func(params ExpandPromptParams, sink func(*ExpandDecisionPayload)) error {
		// 必须只看到 SKELETON 的(aaaa)
		if len(params.Skeletons) != 1 || params.Skeletons[0].ID != "aaaa" {
			t.Errorf("expand pass should only see SKELETON chunks, got %+v", params.Skeletons)
		}
		sink(&ExpandDecisionPayload{Upgrades: []ExpandItem{
			{ChunkID: "aaaa", Reason: "needs full code"},
		}})
		return nil
	}
	sel, _ := NewSelector(SelectorConfig{
		Triage:       triage,
		Expand:       expand,
		EnableExpand: true,
	})

	decisions, _ := sel.Select("x", chunks)
	if decisions["aaaa"].Mode != ModeFull {
		t.Errorf("A should be upgraded to FULL, got %s", decisions["aaaa"].Mode)
	}
	if decisions["bbbb"].Mode != ModeIgnore {
		t.Errorf("B should stay IGNORE, got %s", decisions["bbbb"].Mode)
	}
}

func TestSelect_ExpandCannotDowngrade(t *testing.T) {
	// 保护属性:Pass2 不能把 Pass1 已定的 FULL 降成别的
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A"},
	}
	triage := func(p TriagePromptParams, s func(*TriageDecisionPayload)) error {
		s(&TriageDecisionPayload{Decisions: []TriageItem{{ChunkID: "aaaa", Mode: "FULL"}}})
		return nil
	}
	// Pass2 没有 SKELETON,不该被调用;但就算调了它也只能升级不能降级
	expand := func(p ExpandPromptParams, s func(*ExpandDecisionPayload)) error {
		// Pass2 upgrades 里只能放"升级"项,这里我们假装 LLM 疯了乱发
		s(&ExpandDecisionPayload{Upgrades: []ExpandItem{
			{ChunkID: "aaaa"}, // 已经是 FULL,再发这一项应当 no-op
		}})
		return nil
	}
	sel, _ := NewSelector(SelectorConfig{Triage: triage, Expand: expand, EnableExpand: true})

	decisions, _ := sel.Select("x", chunks)
	if decisions["aaaa"].Mode != ModeFull {
		t.Errorf("want FULL, got %s", decisions["aaaa"].Mode)
	}
}

func TestSelect_ExpandFailureDoesntBreakPass1(t *testing.T) {
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A"},
	}
	triage := func(p TriagePromptParams, s func(*TriageDecisionPayload)) error {
		s(&TriageDecisionPayload{Decisions: []TriageItem{{ChunkID: "aaaa", Mode: "SKELETON"}}})
		return nil
	}
	expand := func(p ExpandPromptParams, s func(*ExpandDecisionPayload)) error {
		return fmt.Errorf("network blip")
	}
	sel, _ := NewSelector(SelectorConfig{Triage: triage, Expand: expand, EnableExpand: true})

	decisions, report := sel.Select("x", chunks)
	if !report.ExpandErrored {
		t.Errorf("expand error not recorded")
	}
	// Pass1 结果被保留
	if decisions["aaaa"].Mode != ModeSkeleton {
		t.Errorf("Pass1 result should survive expand failure, got %s", decisions["aaaa"].Mode)
	}
}

// --- Renderer 测试 ---

func TestRenderer_ThreeSectionsEmitted(t *testing.T) {
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A", FilePath: "a.go", Kind: chunk.KindFunction,
			Skeleton: "func A() { /* ... */ }", Body: "func A() { do_a() }"},
		{ID: "bbbb", Name: "B", FilePath: "b.go", Kind: chunk.KindFunction,
			Skeleton: "func B() { /* ... */ }", Body: "func B() { do_b() }"},
		{ID: "cccc", Name: "C", FilePath: "c.go", Kind: chunk.KindFunction,
			Skeleton: "func C() { /* ... */ }", Body: "func C() { do_c() }"},
	}
	decisions := DecisionMap{
		"aaaa": {ChunkID: "aaaa", Mode: ModeFull},
		"bbbb": {ChunkID: "bbbb", Mode: ModeSkeleton},
		"cccc": {ChunkID: "cccc", Mode: ModeIgnore},
	}
	r := NewRenderer()
	out := r.Render(chunks, decisions)

	// 三段都有
	for _, tag := range []string{"<ignored_chunks", "<skeleton_chunks", "<full_chunks"} {
		if !strings.Contains(out, tag) {
			t.Errorf("missing section %s:\n%s", tag, out)
		}
	}
	// ignored 段只给计数
	if !strings.Contains(out, `count="1"`) {
		t.Errorf("ignored count wrong:\n%s", out)
	}
	// A 的完整 body 出现在 full 段
	if !strings.Contains(out, "func A() { do_a() }") {
		t.Errorf("A body missing:\n%s", out)
	}
	// B 的 skeleton 出现,但不应有 body
	if !strings.Contains(out, "func B() { /* ... */ }") {
		t.Errorf("B skeleton missing:\n%s", out)
	}
	if strings.Contains(out, "do_b()") {
		t.Errorf("B body leaked into skeleton section:\n%s", out)
	}
	// C 的任何内容都不应出现(只计数)
	if strings.Contains(out, "do_c()") {
		t.Errorf("ignored C body leaked:\n%s", out)
	}
}

func TestRenderer_EmptySections(t *testing.T) {
	// 只有 full,没 skeleton 没 ignored
	chunks := []*chunk.Chunk{
		{ID: "aaaa", Name: "A", FilePath: "a.go", Body: "body"},
	}
	decisions := DecisionMap{"aaaa": {ChunkID: "aaaa", Mode: ModeFull}}
	out := NewRenderer().Render(chunks, decisions)

	// 空段仍发出(count="0")
	if !strings.Contains(out, `<skeleton_chunks count="0"`) {
		t.Errorf("empty skeleton section not emitted:\n%s", out)
	}
	if !strings.Contains(out, `<ignored_chunks count="0"`) {
		t.Errorf("empty ignored section not emitted:\n%s", out)
	}
}

// --- Gateway 门面端到端 ---

func TestGateway_EndToEnd(t *testing.T) {
	st := newStubStore(
		mkChunk("aaaa", "Hello", "main.go", chunk.KindFunction, "func Hello() { world() }"),
		mkChunk("bbbb", "world", "util.go", chunk.KindFunction, "func world() {}"),
		mkChunk("cccc", "Goodbye", "other.go", chunk.KindFunction, "func Goodbye() {}"),
	)
	st.chunks["aaaa"].Refs = []string{"world"}

	sel, _ := NewSelector(SelectorConfig{
		ShardSize:   10,
		Concurrency: 2,
		Triage:      namingTriageCaller(t),
	})
	gw, err := New(st, sel, nil)
	if err != nil {
		t.Fatal(err)
	}

	br, err := gw.BuildContext("change the Hello function greeting")
	if err != nil {
		t.Fatal(err)
	}

	// Hello 是 FULL,world 是 SKELETON,Goodbye 是 IGNORE
	if br.Decisions["aaaa"].Mode != ModeFull {
		t.Errorf("Hello should be FULL")
	}
	if br.Decisions["bbbb"].Mode != ModeSkeleton {
		t.Errorf("world should be SKELETON (via refs of Hello)")
	}
	if br.Decisions["cccc"].Mode != ModeIgnore {
		t.Errorf("Goodbye should be IGNORE")
	}

	// Prompt 字符串应包含 Hello 的完整 body
	if !strings.Contains(br.Prompt, "world()") {
		t.Errorf("Hello body should appear in prompt:\n%s", br.Prompt)
	}
	// 不应包含 Goodbye 的 body
	if strings.Contains(br.Prompt, "Goodbye() {}") {
		t.Errorf("Goodbye body should be ignored:\n%s", br.Prompt)
	}
}
