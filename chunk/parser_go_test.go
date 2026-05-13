package chunk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 一个覆盖多种声明形式的 fixture。
// 用 Go 写的代码,但故意包含边界情况:
//   - 顶层函数 + 方法(值接收者、指针接收者、泛型接收者)
//   - struct / interface / type alias
//   - 带 doc comment 的声明
//   - 多行签名 + 泛型参数
const goFixture = `// Package demo 用于 parser 测试。
package demo

import "fmt"

// User 代表一个用户。
type User struct {
	Name string
	Age  int
}

// Saver 描述可存储对象。
type Saver interface {
	Save() error
}

// Alias 类型别名。
type Alias = string

// Greet 打个招呼。
func Greet(name string) string {
	return fmt.Sprintf("hi %s", name)
}

// Save 保存用户到磁盘。
func (u *User) Save() error {
	return doSave(u.Name)
}

// String 返回字符串形式。
func (u User) String() string {
	return u.Name
}

// GenericMap 是泛型容器。
type GenericMap[K comparable, V any] struct {
	data map[K]V
}

// Put 插入键值对。
func (m *GenericMap[K, V]) Put(k K, v V) {
	m.data[k] = v
}

func doSave(n string) error { return nil }
`

// writeFixture 把 fixture 写到临时文件。
func writeFixture(t *testing.T, content string) (absPath, relPath string) {
	t.Helper()
	dir := t.TempDir()
	abs := filepath.Join(dir, "demo.go")
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return abs, "demo.go"
}

func TestParseGoFile_AllDeclsCaptured(t *testing.T) {
	abs, rel := writeFixture(t, goFixture)
	chunks, err := ParseGoFile(abs, rel)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 预期的 Name 列表
	want := map[string]Kind{
		"User":           KindStruct,
		"Saver":          KindInterface,
		"Alias":          KindType,
		"Greet":          KindFunction,
		"User.Save":      KindMethod,
		"User.String":    KindMethod,
		"GenericMap":     KindStruct,
		"GenericMap.Put": KindMethod,
		"doSave":         KindFunction,
	}

	got := map[string]Kind{}
	for _, c := range chunks {
		got[c.Name] = c.Kind
	}

	for name, kind := range want {
		actualKind, ok := got[name]
		if !ok {
			t.Errorf("missing chunk: %s", name)
			continue
		}
		if actualKind != kind {
			t.Errorf("chunk %s: want kind %s, got %s", name, kind, actualKind)
		}
	}

	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected extra chunk: %s", name)
		}
	}
}

func TestParseGoFile_BodyPreservesDocAndSignature(t *testing.T) {
	abs, rel := writeFixture(t, goFixture)
	chunks, err := ParseGoFile(abs, rel)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 找 Greet
	var greet *Chunk
	for _, c := range chunks {
		if c.Name == "Greet" {
			greet = c
			break
		}
	}
	if greet == nil {
		t.Fatal("Greet not found")
	}

	// Body 必须包含 doc comment、完整签名、以及函数体
	if !strings.Contains(greet.Body, "// Greet 打个招呼。") {
		t.Errorf("body missing doc comment:\n%s", greet.Body)
	}
	if !strings.Contains(greet.Body, "func Greet(name string) string") {
		t.Errorf("body missing signature:\n%s", greet.Body)
	}
	if !strings.Contains(greet.Body, "fmt.Sprintf") {
		t.Errorf("body missing implementation:\n%s", greet.Body)
	}
}

func TestParseGoFile_GenericReceiverNameExtraction(t *testing.T) {
	abs, rel := writeFixture(t, goFixture)
	chunks, err := ParseGoFile(abs, rel)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// 泛型方法 receiver 必须正确剥出 "GenericMap",而不是 "GenericMap[K, V]"
	found := false
	for _, c := range chunks {
		if c.Name == "GenericMap.Put" {
			found = true
			if c.Kind != KindMethod {
				t.Errorf("GenericMap.Put: want Method, got %s", c.Kind)
			}
		}
	}
	if !found {
		t.Error("GenericMap.Put not found — generic receiver name extraction broken")
	}
}

func TestParseGoFile_RefsCaptured(t *testing.T) {
	abs, rel := writeFixture(t, goFixture)
	chunks, err := ParseGoFile(abs, rel)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// User.Save 应当引用 doSave
	for _, c := range chunks {
		if c.Name == "User.Save" {
			found := false
			for _, r := range c.Refs {
				if r == "doSave" {
					found = true
				}
			}
			if !found {
				t.Errorf("User.Save.Refs should contain doSave, got %v", c.Refs)
			}
		}
	}
}

func TestParseGoFile_InterfaceMethodNoBody(t *testing.T) {
	// 接口方法没有 body,确保不会 panic 且 Body 完整保留
	src := `package demo

type Reader interface {
	Read(p []byte) (int, error)
}
`
	abs, rel := writeFixture(t, src)
	chunks, err := ParseGoFile(abs, rel)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Kind != KindInterface {
		t.Errorf("want Interface, got %s", chunks[0].Kind)
	}
	if !strings.Contains(chunks[0].Body, "Read(p []byte)") {
		t.Errorf("body missing interface method:\n%s", chunks[0].Body)
	}
}

func TestHashBody_Stable(t *testing.T) {
	h1 := HashBody("hello world")
	h2 := HashBody("hello world")
	h3 := HashBody("hello world!")
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("hash collides on distinct input")
	}
	if len(h1) != 16 {
		t.Errorf("hash length want 16, got %d", len(h1))
	}
}

func TestAllocateID_NoCollision(t *testing.T) {
	exists := map[string]struct{}{}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := AllocateID(exists)
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id produced: %s", id)
		}
		seen[id] = true
		exists[id] = struct{}{}
		if len(id) != 4 {
			t.Errorf("id length want 4, got %d (id=%q)", len(id), id)
		}
	}
}
