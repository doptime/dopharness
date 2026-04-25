package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- L0 MetaRules ---

func TestMetaRulesLayer_FileReadAndCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.md")
	if err := os.WriteFile(path, []byte("rule 1\nrule 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &MetaRulesLayer{Path: path}

	sys, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if usr != "" {
		t.Errorf("rules should go to system, got user: %s", usr)
	}
	if !strings.Contains(sys, "rule 1") || !strings.Contains(sys, "rule 2") {
		t.Errorf("rules content missing:\n%s", sys)
	}
	if !strings.Contains(sys, "<meta_rules>") {
		t.Errorf("wrapper missing:\n%s", sys)
	}

	// 第二次调用应命中缓存(我们验证可用即可;内部 mtime 未变)
	sys2, _, _ := l.Render(&RenderCtx{})
	if sys2 != sys {
		t.Errorf("cache inconsistent")
	}
}

func TestMetaRulesLayer_MissingFileUsesFallback(t *testing.T) {
	l := &MetaRulesLayer{
		Path:     "/nonexistent/nowhere.md",
		Fallback: "default rule",
	}
	sys, _, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if !strings.Contains(sys, "default rule") {
		t.Errorf("fallback not used: %s", sys)
	}
}

func TestMetaRulesLayer_NoPathNoFallback(t *testing.T) {
	l := &MetaRulesLayer{}
	sys, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatal(err)
	}
	if sys != "" || usr != "" {
		t.Errorf("empty layer should emit nothing, got sys=%q usr=%q", sys, usr)
	}
}

// --- L1 InsightIndex ---

func TestInsightIndexLayer_WrapsGatewayOutput(t *testing.T) {
	l := &InsightIndexLayer{}
	ctx := &RenderCtx{
		ContextFromGateway: `<full_chunks><chunk id="aaaa">body</chunk></full_chunks>`,
	}
	sys, usr, err := l.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sys != "" {
		t.Errorf("insight should go to user, got system: %s", sys)
	}
	if !strings.Contains(usr, "<project_context>") {
		t.Errorf("wrapper missing:\n%s", usr)
	}
	if !strings.Contains(usr, `id="aaaa"`) {
		t.Errorf("gateway content missing:\n%s", usr)
	}
}

func TestInsightIndexLayer_EmptyContext(t *testing.T) {
	l := &InsightIndexLayer{}
	sys, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatal(err)
	}
	if sys != "" || usr != "" {
		t.Errorf("empty context should emit nothing")
	}
}

// --- L2 GlobalFacts ---

func TestGlobalFactsLayer_GoesToSystem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "facts.md")
	os.WriteFile(path, []byte("uses Go 1.22"), 0o644)

	l := &GlobalFactsLayer{Path: path}
	sys, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatal(err)
	}
	if usr != "" {
		t.Errorf("facts should go to system, not user")
	}
	if !strings.Contains(sys, "uses Go 1.22") {
		t.Errorf("facts content missing:\n%s", sys)
	}
	if !strings.Contains(sys, "<global_facts>") {
		t.Errorf("wrapper missing")
	}
}

// --- L3 TaskSkills ---

func TestTaskSkillsLayer_LoadsAllMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "how_to_add_route.md"), []byte("step 1: edit router.go"), 0o644)
	os.WriteFile(filepath.Join(dir, "how_to_run_tests.md"), []byte("go test ./..."), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("not a skill"), 0o644) // 应被忽略

	l := &TaskSkillsLayer{Dir: dir}
	_, usr, err := l.Render(&RenderCtx{UserPrompt: "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(usr, "how_to_add_route") {
		t.Errorf("skill 1 missing:\n%s", usr)
	}
	if !strings.Contains(usr, "how_to_run_tests") {
		t.Errorf("skill 2 missing:\n%s", usr)
	}
	if strings.Contains(usr, "not a skill") {
		t.Errorf(".txt file leaked:\n%s", usr)
	}
	if !strings.Contains(usr, `count="2"`) {
		t.Errorf("count wrong:\n%s", usr)
	}
}

func TestTaskSkillsLayer_CustomSelectorFilters(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "route.md"), []byte("for routing"), 0o644)
	os.WriteFile(filepath.Join(dir, "db.md"), []byte("for db"), 0o644)

	// 只挑 name 在 prompt 里出现的
	sel := func(prompt string, all []*Skill) []*Skill {
		var out []*Skill
		for _, s := range all {
			if strings.Contains(prompt, s.Name) {
				out = append(out, s)
			}
		}
		return out
	}
	l := &TaskSkillsLayer{Dir: dir, Selector: sel}

	_, usr, err := l.Render(&RenderCtx{UserPrompt: "i want to add a route"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(usr, "for routing") {
		t.Errorf("route skill should appear:\n%s", usr)
	}
	if strings.Contains(usr, "for db") {
		t.Errorf("db skill should be filtered out:\n%s", usr)
	}
}

func TestTaskSkillsLayer_EmptyDir(t *testing.T) {
	l := &TaskSkillsLayer{Dir: "/does/not/exist"}
	sys, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatal(err)
	}
	if sys != "" || usr != "" {
		t.Errorf("nonexistent dir should emit nothing")
	}
}

// --- L4 SessionRecords ---

func TestSessionRecordsLayer_AppendAndRender(t *testing.T) {
	dir := t.TempDir()
	l := &SessionRecordsLayer{Dir: dir, Limit: 3}

	now := time.Now().Unix()
	// 写 5 条记录,时间戳递增
	for i := 0; i < 5; i++ {
		rec := &SessionRecord{
			ID:        "sess-" + itoa(i),
			Timestamp: now + int64(i),
			Summary:   "did thing " + itoa(i),
		}
		if err := l.Append(rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	_, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatal(err)
	}
	// 应该只有最新 3 条 (id 2, 3, 4)
	if !strings.Contains(usr, `count="3"`) {
		t.Errorf("count wrong:\n%s", usr)
	}
	if !strings.Contains(usr, "did thing 4") {
		t.Errorf("latest (4) missing:\n%s", usr)
	}
	if strings.Contains(usr, "did thing 0") {
		t.Errorf("oldest (0) should be cut:\n%s", usr)
	}
}

func TestSessionRecordsLayer_AppendPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	l := &SessionRecordsLayer{Dir: dir}
	rec := &SessionRecord{ID: "s1", Timestamp: 1000, Summary: "hi"}
	if err := l.Append(rec); err != nil {
		t.Fatal(err)
	}
	// 直接读磁盘验证
	data, err := os.ReadFile(filepath.Join(dir, "s1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back SessionRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Summary != "hi" {
		t.Errorf("persisted content wrong: %+v", back)
	}
}

func TestSessionRecordsLayer_CorruptJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	// 一个合法,一个损坏
	os.WriteFile(filepath.Join(dir, "good.json"), []byte(`{"id":"g","ts":100,"summary":"ok"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`not json !!`), 0o644)

	l := &SessionRecordsLayer{Dir: dir}
	_, usr, err := l.Render(&RenderCtx{})
	if err != nil {
		t.Fatalf("corrupt file should not break render: %v", err)
	}
	if !strings.Contains(usr, "ok") {
		t.Errorf("good record missing:\n%s", usr)
	}
}

// --- Memory 聚合 ---

func TestMemory_OrderAndSectionSplit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rules.md"), []byte("R0"), 0o644)
	os.WriteFile(filepath.Join(dir, "facts.md"), []byte("F2"), 0o644)
	os.MkdirAll(filepath.Join(dir, "skills"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills/s.md"), []byte("S3"), 0o644)

	m := BuildStandardMemory(LayoutDirs{Root: dir})
	sys, usr, errs := m.Render(&RenderCtx{
		UserPrompt:         "do it",
		ContextFromGateway: "<full_chunks></full_chunks>",
	})
	if len(errs) != 0 {
		t.Errorf("unexpected errs: %v", errs)
	}

	// system 应有:L0 rules + L2 facts
	if !strings.Contains(sys, "R0") {
		t.Errorf("L0 missing from system:\n%s", sys)
	}
	if !strings.Contains(sys, "F2") {
		t.Errorf("L2 missing from system:\n%s", sys)
	}
	// L0 应在 L2 之前(L0 是最核心的)
	if strings.Index(sys, "R0") > strings.Index(sys, "F2") {
		t.Errorf("order wrong, L0 should precede L2:\n%s", sys)
	}

	// user 应有:L1(gateway) + L3(skill)
	if !strings.Contains(usr, "<full_chunks>") {
		t.Errorf("L1 missing from user:\n%s", usr)
	}
	if !strings.Contains(usr, "S3") {
		t.Errorf("L3 missing from user:\n%s", usr)
	}
	// 不应把 system 内容泄漏到 user
	if strings.Contains(usr, "R0") {
		t.Errorf("L0 leaked into user:\n%s", usr)
	}
}

func TestMemory_EmptyLayersNoError(t *testing.T) {
	m := &Memory{} // 全 nil
	sys, usr, errs := m.Render(&RenderCtx{})
	if len(errs) != 0 {
		t.Errorf("nil layers should not error: %v", errs)
	}
	if sys != "" || usr != "" {
		t.Errorf("empty memory should emit nothing: sys=%q usr=%q", sys, usr)
	}
}

func TestMemory_DefaultRulesFallback(t *testing.T) {
	// 没 rules.md,但用 BuildStandardMemory 应当拿到 Fallback
	dir := t.TempDir() // 空目录
	m := BuildStandardMemory(LayoutDirs{Root: dir})
	sys, _, _ := m.Render(&RenderCtx{})
	// DefaultRulesFallback 包含 "Tool Call"
	if !strings.Contains(sys, "Tool Call") {
		t.Errorf("default rules fallback not used:\n%s", sys)
	}
}

// 辅助:避免 import strconv
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	n := i
	if n < 0 {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if i < 0 {
		s = "-" + s
	}
	return s
}
