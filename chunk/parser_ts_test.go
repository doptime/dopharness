package chunk

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TS parser 的集成测试依赖 bun 或等效环境。
//
// 本地开发/CI 环境:
//   - 默认:查找 PATH 里的 `bun`,没有就 SKIP
//   - DOPHARNESS_BUN=<path> 可指定自定义 bun 或 shim
//
// 在这个容器里我们没有 bun,所以用 npx + tsx 做 shim(见 fake-bun.sh)。
// 生产部署必须用真正的 Bun。

// skipIfNoRuntime 检查测试环境是否具备 TS 解析能力,否则跳过。
func skipIfNoRuntime(t *testing.T) {
	t.Helper()
	if override := os.Getenv("DOPHARNESS_BUN"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return
		}
	}
	if _, err := exec.LookPath("bun"); err == nil {
		return
	}
	t.Skip("TS parser test: neither $DOPHARNESS_BUN nor `bun` available — skipping")
}

// setupTSFixture 写一个标准的 TS 测试文件,返回其绝对路径。
func setupTSFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	content := `// User represents a person.
export interface User {
    name: string;
    age: number;
}

type ID = string | number;

/**
 * Greet someone.
 */
export function greet(name: string): string {
    return ` + "`hi ${name}`" + `;
}

export class UserService<T extends User> {
    private cache: Map<ID, T> = new Map();

    async add(user: T): Promise<void> {
        this.cache.set(user.name, user);
    }

    get count(): number {
        return this.cache.size;
    }
}

export const handler = async (req: Request): Promise<Response> => {
    return new Response("ok");
};
`
	path := filepath.Join(dir, "fixture.ts")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestTSParser_ParseSingleFile(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	abs := setupTSFixture(t)
	chunks, err := p.ParseTSFile(abs, "fixture.ts")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := map[string]Kind{}
	for _, c := range chunks {
		got[c.Name] = c.Kind
	}

	// 期望:User(Interface) + ID(Type) + greet(Function) + UserService(Class)
	//       + UserService.add(Method) + UserService.get count(Method) + handler(Function)
	want := map[string]Kind{
		"User":                  KindInterface,
		"ID":                    KindType,
		"greet":                 KindFunction,
		"UserService":           KindClass,
		"UserService.add":       KindMethod,
		"UserService.get count": KindMethod,
		"handler":               KindFunction,
	}

	for name, kind := range want {
		actualKind, ok := got[name]
		if !ok {
			t.Errorf("missing chunk: %q", name)
			continue
		}
		if actualKind != kind {
			t.Errorf("chunk %q: want kind %s, got %s", name, kind, actualKind)
		}
	}
	// 允许多出少量的 chunk(例如 TS AST 里可能解析出的其他边缘节点),
	// 只报告真正"意外"的:检查 got 里每一项要么属于 want,要么有合理解释
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Logf("extra chunk produced (not failing): %q", name)
		}
	}
}

func TestTSParser_SkeletonPreservesGenerics(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	abs := setupTSFixture(t)
	chunks, err := p.ParseTSFile(abs, "fixture.ts")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// UserService 的骨架必须保留泛型约束,不能被 indexOf('{') 坑到
	var us *Chunk
	for _, c := range chunks {
		if c.Name == "UserService" {
			us = c
			break
		}
	}
	if us == nil {
		t.Fatal("UserService not found")
	}
	if !strings.Contains(us.Skeleton, "UserService<T extends User>") {
		t.Errorf("skeleton lost generic constraint:\n%s", us.Skeleton)
	}
	if !strings.Contains(us.Skeleton, "/* ... */") {
		t.Errorf("skeleton missing body placeholder:\n%s", us.Skeleton)
	}
	// class 体内的 cache 字段不应在 skeleton 中
	if strings.Contains(us.Skeleton, "private cache") {
		t.Errorf("skeleton leaked class body:\n%s", us.Skeleton)
	}
}

func TestTSParser_ArrowFunctionCaptured(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	abs := setupTSFixture(t)
	chunks, err := p.ParseTSFile(abs, "fixture.ts")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var h *Chunk
	for _, c := range chunks {
		if c.Name == "handler" {
			h = c
			break
		}
	}
	if h == nil {
		t.Fatal("handler (arrow function) not captured")
	}
	if h.Kind != KindFunction {
		t.Errorf("handler: want Function kind, got %s", h.Kind)
	}
	if !strings.Contains(h.Skeleton, "async (req: Request)") {
		t.Errorf("handler skeleton doesn't preserve signature:\n%s", h.Skeleton)
	}
}

func TestTSParser_MethodsQualifiedWithClassName(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	abs := setupTSFixture(t)
	chunks, err := p.ParseTSFile(abs, "fixture.ts")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 原 analyzer.js 的 bug:方法用裸名 "add",会和顶层 function add 撞
	// 新版必须用 "UserService.add"
	for _, c := range chunks {
		if c.Kind == KindMethod && !strings.Contains(c.Name, ".") {
			t.Errorf("method %q missing class qualifier", c.Name)
		}
	}
}

func TestTSParser_BatchMode(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	dir := t.TempDir()

	// 写两个不同的文件
	a := filepath.Join(dir, "a.ts")
	b := filepath.Join(dir, "b.ts")
	os.WriteFile(a, []byte(`export function one() { return 1; }`), 0o644)
	os.WriteFile(b, []byte(`export function two() { return 2; }`), 0o644)

	results, err := p.ParseTSFiles([]TSFileReq{
		{AbsPath: a, RelPath: "a.ts"},
		{AbsPath: b, RelPath: "b.ts"},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// 结果次序不保证,按 RelPath 排序
	sort.Slice(results, func(i, j int) bool { return results[i].RelPath < results[j].RelPath })

	if results[0].Err != nil || len(results[0].Chunks) != 1 || results[0].Chunks[0].Name != "one" {
		t.Errorf("a.ts wrong: err=%v chunks=%+v", results[0].Err, results[0].Chunks)
	}
	if results[1].Err != nil || len(results[1].Chunks) != 1 || results[1].Chunks[0].Name != "two" {
		t.Errorf("b.ts wrong: err=%v chunks=%+v", results[1].Err, results[1].Chunks)
	}

	// 每个 chunk 的 FilePath 必须是 RelPath,不是 AbsPath
	if results[0].Chunks[0].FilePath != "a.ts" {
		t.Errorf("FilePath want 'a.ts', got %q", results[0].Chunks[0].FilePath)
	}
}

func TestTSParser_NonexistentFile(t *testing.T) {
	skipIfNoRuntime(t)

	p := NewTSParser()
	// 不存在的文件:sidecar 层面捕获错误,不应让整批失败
	results, err := p.ParseTSFiles([]TSFileReq{
		{AbsPath: "/nonexistent/foo.ts", RelPath: "foo.ts"},
	})
	if err != nil {
		t.Fatalf("batch should not fail: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Error("expected per-file Err to be set for missing file")
	}
}
