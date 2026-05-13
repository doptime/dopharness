package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/doptime/dopharness/chunk"
)

// mkChunk 是测试里快速构造 chunk 的辅助函数。
func mkChunk(name string, kind chunk.Kind, body string) *chunk.Chunk {
	return &chunk.Chunk{
		Name: name,
		Kind: kind,
		Body: body,
	}
}

func TestJSONStore_LoadEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(s.AllChunks()) != 0 {
		t.Errorf("want empty store")
	}
}

func TestJSONStore_UpsertAllocatesID(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	chunks := []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "func Foo() {}"),
		mkChunk("Bar", chunk.KindFunction, "func Bar() {}"),
	}

	result, err := s.UpsertFile("a.go", "hash1", 1000, chunks)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(result))
	}
	for _, c := range result {
		if c.ID == "" {
			t.Errorf("chunk %s has empty ID", c.Name)
		}
		if len(c.ID) != 4 {
			t.Errorf("chunk %s ID length want 4, got %d", c.Name, len(c.ID))
		}
		if c.UpdatedAt == 0 {
			t.Errorf("chunk %s has zero UpdatedAt", c.Name)
		}
	}

	if result[0].ID == result[1].ID {
		t.Errorf("duplicate ID allocated: %s", result[0].ID)
	}
}

// 关键测试:ID 稳定性 —— 同一个文件第二次 upsert(内容可能变了),
// 只要 Name+Kind 匹配,ID 必须复用。
func TestJSONStore_UpsertReusesIDByNameKind(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	// 第一次:Foo + Bar
	first, _ := s.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "func Foo() { v1 }"),
		mkChunk("Bar", chunk.KindFunction, "func Bar() {}"),
	})
	fooID := findID(first, "Foo")
	barID := findID(first, "Bar")

	// 第二次:Foo 内容变了, Bar 不变
	second, _ := s.UpsertFile("a.go", "h2", 2000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "func Foo() { v2 modified }"),
		mkChunk("Bar", chunk.KindFunction, "func Bar() {}"),
	})

	if findID(second, "Foo") != fooID {
		t.Errorf("Foo ID changed: %s -> %s", fooID, findID(second, "Foo"))
	}
	if findID(second, "Bar") != barID {
		t.Errorf("Bar ID changed: %s -> %s", barID, findID(second, "Bar"))
	}

	// Foo 的 body 必须真变了
	if c, _ := s.GetChunk(fooID); c.Body == first[0].Body {
		t.Errorf("Foo body unchanged despite update")
	}
}

// 关键测试:删除场景 —— 第二次 upsert 没带 Bar,Bar 必须消失。
func TestJSONStore_UpsertDropsRemovedChunks(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	first, _ := s.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "F1"),
		mkChunk("Bar", chunk.KindFunction, "B1"),
	})
	barID := findID(first, "Bar")

	_, _ = s.UpsertFile("a.go", "h2", 2000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "F1"),
	})

	if _, ok := s.GetChunk(barID); ok {
		t.Errorf("Bar should be dropped after upsert without it")
	}
	if len(s.ChunksByFile("a.go")) != 1 {
		t.Errorf("want 1 chunk in a.go, got %d", len(s.ChunksByFile("a.go")))
	}
}

// 关键测试:同名不同类型 —— 一个文件里同时有 type User 和 func User(),
// 必须分配两个独立 ID,不能混用。
func TestJSONStore_UpsertDistinguishesByKind(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	result, _ := s.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("User", chunk.KindStruct, "type User struct{}"),
		mkChunk("User", chunk.KindFunction, "func User() {}"),
	})
	if len(result) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(result))
	}
	if result[0].ID == result[1].ID {
		t.Errorf("same ID for different-kind chunks")
	}
}

// 关键测试:ChunksByFile 保留源码顺序。
func TestJSONStore_ChunksByFilePreservesOrder(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	_, _ = s.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("First", chunk.KindFunction, "F"),
		mkChunk("Second", chunk.KindFunction, "S"),
		mkChunk("Third", chunk.KindFunction, "T"),
	})

	got := s.ChunksByFile("a.go")
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(got))
	}
	wantOrder := []string{"First", "Second", "Third"}
	for i, name := range wantOrder {
		if got[i].Name != name {
			t.Errorf("position %d: want %s, got %s", i, name, got[i].Name)
		}
	}
}

func TestJSONStore_DeleteFile(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONStore(dir)
	_ = s.Load()

	first, _ := s.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "F"),
	})
	fooID := first[0].ID

	if err := s.DeleteFile("a.go"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := s.GetChunk(fooID); ok {
		t.Error("chunk should be gone after DeleteFile")
	}
	if _, ok := s.GetFileMeta("a.go"); ok {
		t.Error("file meta should be gone")
	}
	// 幂等
	if err := s.DeleteFile("a.go"); err != nil {
		t.Errorf("second delete should be noop, got: %v", err)
	}
}

// 关键测试:持久化 —— Flush 后,新的 store 实例从同一目录 Load 应得到相同数据。
func TestJSONStore_FlushThenReload(t *testing.T) {
	dir := t.TempDir()
	s1 := NewJSONStore(dir)
	_ = s1.Load()

	first, _ := s1.UpsertFile("a.go", "h1", 1000, []*chunk.Chunk{
		mkChunk("Foo", chunk.KindFunction, "F"),
		mkChunk("Bar", chunk.KindFunction, "B"),
	})
	fooID := findID(first, "Foo")

	if err := s1.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// 单一持久化文件:只检查 files.json
	if _, err := os.Stat(filepath.Join(dir, "files.json")); err != nil {
		t.Errorf("files.json not written: %v", err)
	}
	// chunks.json 不再存在(消融简化的一部分)
	if _, err := os.Stat(filepath.Join(dir, "chunks.json")); err == nil {
		t.Errorf("chunks.json should no longer be written (merged into files.json)")
	}

	// 新实例重读
	s2 := NewJSONStore(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s2.AllChunks()) != 2 {
		t.Errorf("reloaded count want 2, got %d", len(s2.AllChunks()))
	}
	if c, ok := s2.GetChunk(fooID); !ok {
		t.Errorf("Foo ID %s lost after reload", fooID)
	} else if c.Name != "Foo" {
		t.Errorf("Foo chunk corrupted: name=%s", c.Name)
	}
	if len(s2.ChunksByFile("a.go")) != 2 {
		t.Errorf("ChunksByFile not rebuilt on load")
	}
}

// findID 从结果切片中按 Name 取 ID。
func findID(chunks []*chunk.Chunk, name string) string {
	for _, c := range chunks {
		if c.Name == name {
			return c.ID
		}
	}
	return ""
}
