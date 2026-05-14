// coffee_cloze_v3.go — Coffee Cloze Game 自动审计 + 自愈 CLI (V3)
//
// V2 → V3 变更摘要:
//   - 删除 SECTION 7 (手写 fix loop ~120 行)     → h.RunTask() 一个调用
//   - 删除 SECTION 8 (harness 接线 ~100 行)       → llmadapter.QuickStart 一行
//   - 每次任务自动归档到 L4 (含 LLM 内省 reflections)
//   - 外部代码从 ~1000 行降至 ~650 行,领域逻辑零损失
//
// 用法不变:
//   go run coffee_cloze_v3.go -url http://localhost:3000/games/coffee-cloze
//   go run coffee_cloze_v3.go -fix -project-root ./repo -max-rounds 3
//   go run coffee_cloze_v3.go -fix -project-root ./repo -dry-run

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/doptime/llm"

	"github.com/doptime/dopharness/harness"
	"github.com/doptime/dopharness/llmadapter"
)

// ============================================================================
// SECTION 1 · Game-specific expectations (unchanged from V2)
// ============================================================================

type scenarioExpectation struct {
	ID              string
	Tags            []string
	ExpectedVerdict string
	Description     string
}

var coffeeClozeExpectations = []scenarioExpectation{
	{ID: "perfect-player", Tags: []string{"happy-path"}, ExpectedVerdict: "PASS",
		Description: "全 6 题拖对,验证 happy path 与 complete screen"},
	{ID: "position-error-player", Tags: []string{"failure-path", "position-error"}, ExpectedVerdict: "PASS",
		Description: "4 道 scene cloze 拖错 zone,验证 position-error 分类"},
	{ID: "semantic-error-player", Tags: []string{"failure-path", "semantic-error"}, ExpectedVerdict: "PASS",
		Description: "全部拖错词,验证 semantic-error 与 reviewQueue 入队"},
	{ID: "hint-player", Tags: []string{"happy-path", "hint-penalty"}, ExpectedVerdict: "PASS",
		Description: "每题先求助再做对,验证 30% 罚分计算"},
	{ID: "mixed-player", Tags: []string{"regression"}, ExpectedVerdict: "PASS",
		Description: "奇偶交替对错,验证混合输入下的计分与队列"},
}

var canonicalFiles = struct {
	GameFile    string
	ScriptsFile string
}{
	GameFile:    "coffee_cloze_game.tsx",
	ScriptsFile: "examples/coffee-cloze/scripts.ts",
}

// ============================================================================
// SECTION 2 · HTTP / verdict 数据结构 (unchanged from V2)
// ============================================================================

type batchResponse struct {
	OK         bool             `json:"ok"`
	Error      string           `json:"error,omitempty"`
	GameIntent string           `json:"gameIntent"`
	Results    []scenarioResult `json:"results"`
	Verdict    string           `json:"verdict"`
	LLMError   string           `json:"llmError,omitempty"`
	LLMSkipped string           `json:"llmSkipped,omitempty"`
	DurationMs int64            `json:"durationMs"`
}

type scenarioResult struct {
	ScenarioID string          `json:"scenarioId"`
	Hypothesis string          `json:"hypothesis"`
	Intent     string          `json:"intent,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	InfraError string          `json:"infraError,omitempty"`
	DurationMs int64           `json:"durationMs"`
}

type batchVerdict struct {
	PerScenario           []perScenarioVerdict `json:"perScenario"`
	CrossScenarioFindings []crossFinding       `json:"crossScenarioFindings"`
	RootCauses            []rootCause          `json:"rootCauses"`
	Notes                 string               `json:"notes"`
}

type perScenarioVerdict struct {
	ScenarioID string   `json:"scenarioId"`
	Verdict    string   `json:"verdict"`
	Score      int      `json:"score"`
	Evidence   []string `json:"evidence"`
}

type crossFinding struct {
	Pattern           string   `json:"pattern"`
	ScenariosAffected []string `json:"scenariosAffected"`
	Evidence          string   `json:"evidence"`
}

type rootCause struct {
	Issue        string `json:"issue"`
	SuggestedFix struct {
		Target string `json:"target"`
		File   string `json:"file"`
		Change string `json:"change"`
	} `json:"suggestedFix"`
}

type auditResult struct {
	Response   batchResponse
	Verdict    *batchVerdict
	RawVerdict string
	ParseError error
}

// ============================================================================
// SECTION 3 · ANSI 颜色 (unchanged from V2)
// ============================================================================

const (
	cReset   = "\033[0m"
	cBold    = "\033[1m"
	cDim     = "\033[2m"
	cRed     = "\033[31m"
	cGreen   = "\033[32m"
	cYellow  = "\033[33m"
	cBlue    = "\033[34m"
	cMagenta = "\033[35m"
	cCyan    = "\033[36m"
	cGray    = "\033[90m"
)

var useColor = true

func col(s, color string) string {
	if !useColor {
		return s
	}
	return color + s + cReset
}

func verdictColor(v string) string {
	switch v {
	case "PASS":
		return cGreen
	case "PARTIAL":
		return cYellow
	case "FAIL", "INFRA_FAIL":
		return cRed
	default:
		return cGray
	}
}

// ============================================================================
// SECTION 4 · CLI flags (unchanged from V2)
// ============================================================================

type cliConfig struct {
	WebopsAddr   string
	GameURL      string
	ScenariosCsv string
	TagsCsv      string
	Concurrency  int
	SkipLLM      bool
	Timeout      time.Duration
	NoColor      bool
	Raw          bool
	Strict       bool

	FixMode     bool
	ProjectRoot string
	MaxRounds   int
	HMRSettle   time.Duration
	DryRun      bool
	GameFile    string
	ScriptsFile string
}

func parseFlags() cliConfig {
	cfg := cliConfig{}
	flag.StringVar(&cfg.WebopsAddr, "webops", "http://localhost:8080", "webops backend address")
	flag.StringVar(&cfg.GameURL, "url", "http://localhost:3000/games/coffee-cloze", "Coffee Cloze game URL")
	flag.StringVar(&cfg.ScenariosCsv, "scenarios", "", "comma-separated scenario IDs")
	flag.StringVar(&cfg.TagsCsv, "tags", "", "comma-separated tag filter")
	flag.IntVar(&cfg.Concurrency, "concurrency", 0, "server-side concurrency override")
	flag.BoolVar(&cfg.SkipLLM, "no-llm", false, "skip auditor LLM call")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Minute, "overall timeout")
	flag.BoolVar(&cfg.NoColor, "no-color", false, "disable ANSI colors")
	flag.BoolVar(&cfg.Raw, "raw", false, "print raw LLM verdict JSON")
	flag.BoolVar(&cfg.Strict, "strict", false, "exit 1 if any verdict != expected")

	flag.BoolVar(&cfg.FixMode, "fix", false, "enable iterative fix via dopharness RunTask")
	flag.StringVar(&cfg.ProjectRoot, "project-root", "", "path to project root")
	flag.IntVar(&cfg.MaxRounds, "max-rounds", 3, "max audit→fix→audit cycles")
	flag.DurationVar(&cfg.HMRSettle, "hmr-settle", 4*time.Second, "wait for HMR between rounds")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "print description and exit")
	flag.StringVar(&cfg.GameFile, "game-file", canonicalFiles.GameFile, "canonical game source file")
	flag.StringVar(&cfg.ScriptsFile, "scripts-file", canonicalFiles.ScriptsFile, "canonical test script file")
	flag.Parse()

	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		cfg.NoColor = true
	}
	return cfg
}

// ============================================================================
// SECTION 5 · Main
// ============================================================================

func main() {
	cfg := parseFlags()
	useColor = !cfg.NoColor

	if cfg.FixMode {
		if cfg.ProjectRoot == "" {
			fatal("-fix requires -project-root")
		}
		os.Exit(runFixLoop(cfg))
	}
	os.Exit(runSingleAudit(cfg))
}

// ============================================================================
// SECTION 6 · Single-shot audit (unchanged from V2)
// ============================================================================

func runSingleAudit(cfg cliConfig) int {
	printHeader(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	ar, err := runAuditOnce(ctx, cfg)
	if err != nil {
		fatal("audit failed: %v", err)
	}

	if cfg.Raw && ar.RawVerdict != "" {
		fmt.Println(col("── Raw LLM verdict ────────────────────────────", cGray))
		fmt.Println(ar.RawVerdict)
		fmt.Println()
	}
	printRunStats(ar.Response.Results)

	if ar.Verdict != nil {
		return printVerdict(ar.Verdict, cfg.Strict)
	}
	if ar.Response.LLMError != "" {
		fmt.Println(col("[error] LLM: "+ar.Response.LLMError, cRed))
		return 2
	}
	return 0
}

// ============================================================================
// SECTION 7 · Fix loop — V3: 用 RunTask 替换手写循环
// ============================================================================
//
// V2 的 SECTION 7 (~120 行) + SECTION 8 (~100 行) + SECTION 9 (~100 行)
// 合计 ~320 行,现在压缩到 ~60 行。
//
// 核心变化:
//   V2: 手写 for 循环 + 手写 buildHarness + 手写 makeTriageCaller/makeMainCaller/makeToolBuilder
//   V3: llmadapter.QuickStart 一行构造 + h.RunTask 一个调用
//
// RunTask 自动完成:
//   - 验证驱动循环 (初始 verify → LLM 编辑 → re-verify → ... → 全 PASS 或耗尽轮数)
//   - Prompt 自动带反馈历史 (每轮 verify 结果以 <previous_rounds> 追加,LLM 可看全貌)
//   - L4 自动归档 (不论成败;含 LLM 内省 reflections)

func runFixLoop(cfg cliConfig) int {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	printHeader(cfg)
	printFixModeBanner(cfg)

	// ── 构造 Agent ──
	// V2 要写 buildHarness + makeTriageCaller + makeMainCaller + makeToolBuilder (~100 行)
	// V3: 一行
	h, _, err := llmadapter.QuickStart(llmadapter.QuickConfig{
		ProjectRoot: cfg.ProjectRoot,
		Model:       llm.ModelDefault,
		Logger:      func(e string, f map[string]any) { log.Printf("[harness] %s %v", e, f) },
	})
	if err != nil {
		fatal("quickstart: %v", err)
	}
	fmt.Println(col("⏱  Indexing project tree...", cDim))
	if _, err := h.Index(ctx); err != nil {
		fatal("index: %v", err)
	}
	fmt.Println(col("   index ready.", cDim))
	fmt.Println()

	// ── dry-run: 只展示 description ──
	desc := buildFixDescription(cfg)
	if cfg.DryRun {
		fmt.Println(col("── Fix description (dry-run) ──────────────────", cGray))
		fmt.Println(indent(desc, "  "))
		fmt.Println(col("[dry-run] would invoke h.RunTask() here; exiting.", cYellow))
		return 0
	}

	// ── 跑完整闭环 ──
	// V2 要写 for 循环 + stagnation 检测 + prompt 重建 + h.Run + HMR 等待 (~120 行)
	// V3: 一个调用
	result, err := h.RunTask(ctx, harness.Task{
		Description: desc,

		Verify: func(ctx context.Context) (harness.VerifyResult, error) {
			ar, err := runAuditOnce(ctx, cfg)
			if err != nil {
				return harness.VerifyResult{}, err
			}
			return auditToVerifyResult(ar), nil
		},

		BetweenRounds: func(_ context.Context, round int) error {
			fmt.Printf("%s round %d done, waiting %s for HMR...\n",
				col("⏳", cDim), round, cfg.HMRSettle)
			time.Sleep(cfg.HMRSettle)
			return nil
		},

		MaxRounds:  cfg.MaxRounds,
		ArchiveTag: "coffee-cloze-fix",
	})
	if err != nil {
		fmt.Printf("%s RunTask error: %v\n", col("[error]", cRed), err)
		return 2
	}

	// ── 结果报告 ──
	fmt.Println()
	if result.Passed {
		fmt.Println(col(fmt.Sprintf(
			"🎉 All scenarios PASS after %d round(s). Archive: %s",
			result.Rounds, result.ArchiveID), cGreen))
		return 0
	}
	fmt.Println(col(fmt.Sprintf(
		"✗ Did not converge after %d round(s). Archive: %s",
		result.Rounds, result.ArchiveID), cRed))
	if result.LastDiag != "" {
		fmt.Println(col("── Last diagnosis ─────────────────────────────", cGray))
		// 只打摘要,完整数据在 L4 归档里
		printDiagSummary(result.LastDiag)
	}
	if len(result.Reflections) > 0 {
		fmt.Println(col("── LLM reflections ────────────────────────────", cGray))
		for _, r := range result.Reflections {
			fmt.Printf("  %s %s\n", col("·", cCyan), r)
		}
	}
	fmt.Println()
	return 1
}

// buildFixDescription 构造 RunTask.Description — 给 LLM 的常量上下文。
//
// 与 V2 buildFixPrompt 的区别:
//
//	V2: 每轮重建 prompt,把最新 rootCauses 塞进去 (~100 行模板)
//	V3: description 只提供不变的约束和文件位置;变化的 rootCauses
//	    自动通过 RunTask 的 <initial_verification> / <previous_rounds>
//	    段传入 LLM (来自 VerifyResult.Diag)
func buildFixDescription(cfg cliConfig) string {
	return fmt.Sprintf(`The Coffee Cloze game (a React drag-and-drop vocabulary cloze game) needs
automated fixes based on audit results from a test harness.

# Canonical file locations
- Game source:  %s
- Test script:  %s

When the audit says suggestedFix.target == "code", changes go in the game source.
When suggestedFix.target == "script", changes go in the test script.
When suggestedFix.target == "both", coordinated changes in both files.

# Hard constraints
- DO NOT remove or rename any 'data-vt-id="..."' attributes on JSX elements.
- Apply minimal, focused changes resolving the listed root causes only.
- Each modify_chunk call MUST produce syntactically valid TypeScript / TSX.
- If you need to introduce a new function/component, use add_chunk.
- If unsure about existing code, use search_chunks_by_name then read_chunk.
- If the root cause is ambiguous between "code bug" vs "test script bug",
  prefer fixing the test script (less invasive).

The verification feedback below contains per-scenario verdicts, root causes,
and suggested fixes. Apply the fixes using modify_chunk / add_chunk tools.`,
		cfg.GameFile, cfg.ScriptsFile)
}

// auditToVerifyResult 把 audit 产物翻译成 RunTask 的 VerifyResult。
//
// 关键设计:Diag 字段塞入完整的 verdict JSON (含 rootCauses),这样 RunTask
// 的 prompt 构造器会把它以 <initial_verification> / <previous_rounds> 送入
// LLM,LLM 就能看到完整的失败原因和修复建议。
func auditToVerifyResult(ar auditResult) harness.VerifyResult {
	if ar.Verdict == nil {
		diag := "audit did not produce a verdict"
		if ar.Response.LLMError != "" {
			diag += ": " + ar.Response.LLMError
		}
		return harness.VerifyResult{Passed: false, Diag: diag}
	}

	metrics := make([]harness.MetricResult, 0, len(ar.Verdict.PerScenario))
	for _, ps := range ar.Verdict.PerScenario {
		status := harness.VerifyFail
		switch ps.Verdict {
		case "PASS":
			status = harness.VerifyPass
		case "PARTIAL":
			status = harness.VerifyPartial
		case "INFRA_FAIL":
			status = harness.VerifyInfra
		}
		detail := fmt.Sprintf("score=%d", ps.Score)
		if len(ps.Evidence) > 0 {
			detail += "; " + ps.Evidence[0]
		}
		metrics = append(metrics, harness.MetricResult{
			Name:   ps.ScenarioID,
			Status: status,
			Detail: detail,
		})
	}

	diagJSON, _ := json.MarshalIndent(ar.Verdict, "", "  ")
	return harness.VerifyResult{
		Metrics: metrics,
		Diag:    string(diagJSON),
	}
}

// printDiagSummary 从 JSON verdict 里提取 rootCauses 打印摘要。
func printDiagSummary(diagJSON string) {
	var bv batchVerdict
	if json.Unmarshal([]byte(diagJSON), &bv) != nil {
		fmt.Println("  " + diagJSON[:min(200, len(diagJSON))])
		return
	}
	for _, ps := range bv.PerScenario {
		if ps.Verdict != "PASS" {
			fmt.Printf("  %s %-10s %s (score=%d)\n",
				col("●", verdictColor(ps.Verdict)),
				col(ps.Verdict, verdictColor(ps.Verdict)),
				ps.ScenarioID, ps.Score)
		}
	}
	for i, rc := range bv.RootCauses {
		fmt.Printf("  %d. %s (target=%s)\n", i+1, rc.Issue, rc.SuggestedFix.Target)
	}
	fmt.Println()
}

// ============================================================================
// SECTION 10 · Audit 调用 (unchanged from V2)
// ============================================================================

func runAuditOnce(ctx context.Context, cfg cliConfig) (auditResult, error) {
	body := map[string]interface{}{"url": cfg.GameURL, "skipLLM": cfg.SkipLLM}
	if cfg.ScenariosCsv != "" {
		body["scenarios"] = splitCsv(cfg.ScenariosCsv)
	}
	if cfg.TagsCsv != "" {
		body["tags"] = splitCsv(cfg.TagsCsv)
	}
	if cfg.Concurrency > 0 {
		body["concurrency"] = cfg.Concurrency
	}
	bodyBytes, _ := json.Marshal(body)

	endpoint := strings.TrimSuffix(cfg.WebopsAddr, "/") + "/webops/diagnose-batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return auditResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := (&http.Client{Timeout: cfg.Timeout}).Do(req)
	if err != nil {
		return auditResult{}, fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return auditResult{}, fmt.Errorf("backend %d: %s", resp.StatusCode, string(rawBody))
	}

	var batch batchResponse
	if err := json.Unmarshal(rawBody, &batch); err != nil {
		return auditResult{}, fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("%s audit completed in %s (backend %dms)\n",
		col("⏱ ", cDim), time.Since(t0).Round(time.Millisecond), batch.DurationMs)

	ar := auditResult{Response: batch, RawVerdict: batch.Verdict}
	if batch.Verdict != "" {
		v := stripCodeFence(batch.Verdict)
		var bv batchVerdict
		if err := json.Unmarshal([]byte(v), &bv); err != nil {
			ar.ParseError = err
		} else {
			ar.Verdict = &bv
		}
	}
	return ar, nil
}

func stripCodeFence(s string) string {
	v := strings.TrimSpace(s)
	v = strings.TrimPrefix(v, "```json")
	v = strings.TrimPrefix(v, "```")
	v = strings.TrimSuffix(v, "```")
	return strings.TrimSpace(v)
}

// ============================================================================
// SECTION 11 · Pretty printers (simplified from V2, removed harness-report printer)
// ============================================================================

func printHeader(cfg cliConfig) {
	fmt.Println()
	fmt.Println(col("☕ Coffee Cloze Game — Automated Audit (V3)", cYellow))
	fmt.Printf("   backend:   %s\n", col(cfg.WebopsAddr, cBlue))
	fmt.Printf("   game URL:  %s\n", col(cfg.GameURL, cBlue))
	if cfg.ScenariosCsv != "" {
		fmt.Printf("   scenarios: %s\n", col(cfg.ScenariosCsv, cCyan))
	}
	fmt.Println()
}

func printFixModeBanner(cfg cliConfig) {
	fmt.Println(col("── Fix mode (RunTask) ─────────────────────────", cMagenta))
	fmt.Printf("   project root: %s\n", col(cfg.ProjectRoot, cBlue))
	fmt.Printf("   max rounds:   %d\n", cfg.MaxRounds)
	fmt.Printf("   HMR settle:   %s\n", cfg.HMRSettle)
	if cfg.DryRun {
		fmt.Printf("   %s\n", col("DRY RUN", cYellow))
	}
	fmt.Println()
}

func printRunStats(results []scenarioResult) int {
	exitCode := 0
	for _, r := range results {
		var statusText, statusCol string
		switch {
		case r.InfraError != "":
			statusText = "INFRA_FAIL"
			statusCol = cRed
			exitCode = 1
		case len(r.Payload) == 0:
			statusText = "NO_PAYLOAD"
			statusCol = cYellow
		default:
			statusText = "ran"
			statusCol = cGreen
		}
		fmt.Printf("  %s %-28s %s  %s\n",
			col("●", statusCol), r.ScenarioID, col(statusText, statusCol),
			col(fmt.Sprintf("(%dms)", r.DurationMs), cDim))
	}
	fmt.Println()
	return exitCode
}

func printVerdict(bv *batchVerdict, strict bool) int {
	exitCode := 0
	expected := map[string]string{}
	for _, e := range coffeeClozeExpectations {
		expected[e.ID] = e.ExpectedVerdict
	}

	for _, ps := range bv.PerScenario {
		c := verdictColor(ps.Verdict)
		expMark := ""
		if e, ok := expected[ps.ScenarioID]; ok {
			if ps.Verdict == e {
				expMark = col("  ✓", cGreen)
			} else {
				expMark = col(fmt.Sprintf("  ✗ expected %s", e), cRed)
				if strict {
					exitCode = 1
				}
			}
		}
		if ps.Verdict == "FAIL" || ps.Verdict == "INFRA_FAIL" {
			exitCode = 1
		}
		fmt.Printf("  %s %s  %s  score=%d%s\n",
			col("●", c), col(fmt.Sprintf("%-10s", ps.Verdict), c),
			col(ps.ScenarioID, cBold), ps.Score, expMark)
	}
	fmt.Println()

	if len(bv.RootCauses) > 0 {
		fmt.Println(col("── Root causes ────────────────────────────────", cGray))
		for i, rc := range bv.RootCauses {
			fmt.Printf("  %d. %s (target=%s, file=%s)\n",
				i+1, rc.Issue, rc.SuggestedFix.Target, rc.SuggestedFix.File)
		}
		fmt.Println()
	}
	return exitCode
}

// ============================================================================
// SECTION 12 · Utils
// ============================================================================

func splitCsv(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s %s\n", col("[fatal]", cRed), fmt.Sprintf(format, args...))
	os.Exit(2)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
