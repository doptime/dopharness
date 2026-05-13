package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// setupProject 在临时目录里搭一个模拟项目:
//
//	project/
//	  main.go          (2 个 chunk: Hello + User)
//	  util/helper.go   (1 个 chunk: Helper)
//	  frontend/app.ts  (2 个 chunk: render + App)
//	  node_modules/x.go  (应被忽略)
//	  .git/config      (应被忽略)
//	  README.md        (非源码,应被忽略)
func setupProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("main.go", `package main

// Hello greets.
func Hello(name string) string {
	return "hi " + name
}

// User data.
type User struct {
	Name string
}
`)

	mustWrite("util/helper.go", `package util

func Helper() int { return 42 }
`)

	mustWrite("frontend/app.ts", `export function render(x: number): string {
    return String(x);
}

export class App {
    start() { /* boot */ }
}
`)

	// 应被忽略的:
	mustWrite("node_modules/x.go", `package fake
func ShouldNotIndex() {}
`)
	mustWrite(".git/config", "garbage\n")
	mustWrite("README.md", "# demo\n")

	return root
}

func skipIfNoTS(t *testing.T) {
	t.Helper()
	if override := os.Getenv("DOPHARNESS_BUN"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return
		}
	}
	if _, err := exec.LookPath("bun"); err == nil {
		return
	}
	t.Skip("indexer test: neither $DOPHARNESS_BUN nor `bun` available")
}

func TestIndexer_InitialRun(t *testing.T) {
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}

	ix, err := NewIndexer(Config{
		ProjectRoot: root,
		Store:       st,
		TSParser:    chunk.NewTSParser(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ix.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if rep.FilesScanned != 3 {
		t.Errorf("scanned want 3 (main.go, util/helper.go, frontend/app.ts), got %d", rep.FilesScanned)
	}
	if rep.FilesIndexed != 3 {
		t.Errorf("indexed want 3, got %d", rep.FilesIndexed)
	}
	if rep.FilesFailed != 0 {
		t.Errorf("failed want 0, got %d (errors: %v)", rep.FilesFailed, rep.Errors)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("unexpected errors: %v", rep.Errors)
	}

	// 查具体 chunk 数:main.go 2(Hello+User) + helper 1(Helper) + app.ts 3(render+App+App.start) = 6
	allChunks := st.AllChunks()
	if len(allChunks) != 6 {
		t.Errorf("chunks want 6, got %d", len(allChunks))
		for _, c := range allChunks {
			t.Logf("  %s [%s] %s", c.ID, c.Kind, c.Name)
		}
	}

	// 验证忽略:node_modules 里的文件不应产生任何 chunk
	for _, c := range allChunks {
		if strings.Contains(c.FilePath, "node_modules") {
			t.Errorf("node_modules leaked: %s", c.FilePath)
		}
		if strings.Contains(c.FilePath, ".git") {
			t.Errorf(".git leaked: %s", c.FilePath)
		}
	}

	// 验证路径是 POSIX 风格
	for _, c := range allChunks {
		if strings.Contains(c.FilePath, `\`) {
			t.Errorf("backslash in FilePath: %s", c.FilePath)
		}
	}
}

func TestIndexer_IncrementalNoChange(t *testing.T) {
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()

	ix, _ := NewIndexer(Config{ProjectRoot: root, Store: st, TSParser: chunk.NewTSParser()})

	// 第一次:全量
	rep1, _ := ix.Run(context.Background())
	if rep1.FilesIndexed != 3 {
		t.Fatalf("first run expected 3 indexed, got %d", rep1.FilesIndexed)
	}

	// 记录所有 chunk ID,第二次跑后必须全部保留(ID 稳定)
	idsBefore := map[string]bool{}
	for _, c := range st.AllChunks() {
		idsBefore[c.ID] = true
	}

	// 第二次:什么都没改,mtime 命中,全部跳过
	rep2, _ := ix.Run(context.Background())
	if rep2.FilesIndexed != 0 {
		t.Errorf("second run should index 0, got %d", rep2.FilesIndexed)
	}
	if rep2.FilesSkipped != 3 {
		t.Errorf("second run should skip 3, got %d", rep2.FilesSkipped)
	}

	// ID 必须完全一致
	idsAfter := map[string]bool{}
	for _, c := range st.AllChunks() {
		idsAfter[c.ID] = true
	}
	for id := range idsBefore {
		if !idsAfter[id] {
			t.Errorf("ID %s lost on no-op rerun", id)
		}
	}
}

func TestIndexer_IncrementalFileModified(t *testing.T) {
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()

	ix, _ := NewIndexer(Config{ProjectRoot: root, Store: st, TSParser: chunk.NewTSParser()})
	_, _ = ix.Run(context.Background())

	// 抓改前的 Hello chunk ID
	helloID := ""
	for _, c := range st.AllChunks() {
		if c.Name == "Hello" {
			helloID = c.ID
		}
	}
	if helloID == "" {
		t.Fatal("Hello chunk not found after first run")
	}

	// 修改 main.go(让 mtime 一定不同,睡 1 秒保险)
	time.Sleep(1100 * time.Millisecond)
	newContent := `package main

// Hello greets (v2).
func Hello(name string) string {
	return "hello " + name + "!"
}

// User data.
type User struct {
	Name string
}
`
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, _ := ix.Run(context.Background())
	if rep.FilesIndexed != 1 {
		t.Errorf("modified run should index 1, got %d", rep.FilesIndexed)
	}
	if rep.FilesSkipped != 2 {
		t.Errorf("modified run should skip 2, got %d", rep.FilesSkipped)
	}

	// Hello ID 必须和改前一样(ID 稳定性)
	helloAfter := ""
	for _, c := range st.AllChunks() {
		if c.Name == "Hello" {
			helloAfter = c.ID
			// body 必须真的变了
			if !strings.Contains(c.Body, "hello ") || !strings.Contains(c.Body, "!") {
				t.Errorf("Hello body not updated: %s", c.Body)
			}
		}
	}
	if helloAfter != helloID {
		t.Errorf("Hello ID changed %s -> %s (ID stability broken)", helloID, helloAfter)
	}
}

func TestIndexer_DeletedFilePruned(t *testing.T) {
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()

	ix, _ := NewIndexer(Config{ProjectRoot: root, Store: st, TSParser: chunk.NewTSParser()})
	_, _ = ix.Run(context.Background())

	before := len(st.AllChunks())
	if before != 6 {
		t.Fatalf("setup failed: want 6 chunks, got %d", before)
	}

	// 删掉 util/helper.go
	if err := os.Remove(filepath.Join(root, "util/helper.go")); err != nil {
		t.Fatal(err)
	}

	rep, _ := ix.Run(context.Background())
	if rep.FilesRemoved != 1 {
		t.Errorf("removed want 1, got %d", rep.FilesRemoved)
	}
	after := len(st.AllChunks())
	if after != before-1 {
		t.Errorf("chunks want %d after removal, got %d", before-1, after)
	}
	// 具体来说 Helper 必须消失
	for _, c := range st.AllChunks() {
		if c.Name == "Helper" {
			t.Errorf("Helper chunk should be pruned, still found: %s", c.ID)
		}
	}
}

func TestIndexer_FileHashFallbackWhenMtimeChanges(t *testing.T) {
	// 模拟 git checkout 场景:mtime 变了但内容没变,不应重新解析
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()

	ix, _ := NewIndexer(Config{ProjectRoot: root, Store: st, TSParser: chunk.NewTSParser()})
	_, _ = ix.Run(context.Background())

	// 读原内容
	mainGo := filepath.Join(root, "main.go")
	content, err := os.ReadFile(mainGo)
	if err != nil {
		t.Fatal(err)
	}

	// 重写相同内容但让 mtime 变(触碰文件)
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(mainGo, content, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, _ := ix.Run(context.Background())
	// mtime 变了会读文件算 hash,hash 相同,最终不走真解析 → FilesIndexed = 0
	if rep.FilesIndexed != 0 {
		t.Errorf("hash-unchanged rerun should index 0, got %d", rep.FilesIndexed)
	}
	// 这个文件在 skipped 里(其它两个也 skip)
	if rep.FilesSkipped != 3 {
		t.Errorf("hash-unchanged rerun should skip 3, got %d", rep.FilesSkipped)
	}
}

func TestIndexer_UnsupportedFileIgnored(t *testing.T) {
	skipIfNoTS(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.py"), []byte("def foo(): pass"), 0o644)
	os.WriteFile(filepath.Join(root, "b.rs"), []byte("fn bar() {}"), 0o644)
	os.WriteFile(filepath.Join(root, "c.go"), []byte("package c\nfunc C() {}"), 0o644)

	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()
	ix, _ := NewIndexer(Config{ProjectRoot: root, Store: st, TSParser: chunk.NewTSParser()})
	rep, _ := ix.Run(context.Background())

	if rep.FilesScanned != 1 {
		t.Errorf("only c.go should be scanned, got %d", rep.FilesScanned)
	}
	if rep.FilesIndexed != 1 {
		t.Errorf("only c.go should be indexed, got %d", rep.FilesIndexed)
	}
}

func TestIndexer_LoggerCalled(t *testing.T) {
	skipIfNoTS(t)
	root := setupProject(t)
	st := store.NewJSONStore(filepath.Join(root, ".dopharness"))
	_ = st.Load()

	events := map[string]int{}
	logger := func(event, path string, extra map[string]any) {
		events[event]++
	}
	ix, _ := NewIndexer(Config{
		ProjectRoot: root,
		Store:       st,
		TSParser:    chunk.NewTSParser(),
		Logger:      logger,
	})
	_, _ = ix.Run(context.Background())

	if events["indexed"] != 3 {
		t.Errorf("indexed event count want 3, got %d", events["indexed"])
	}
}
