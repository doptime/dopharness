package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/doptime/dopharness/chunk"
)

// JSONStore 是 ChunkStore 的单目录 JSON 实现。
//
// 磁盘布局(只有一张表):
//
//	<dir>/files.json     map[Path]FileMeta   (chunks 内嵌进 FileMeta)
//
// 反向索引 ID -> Chunk 不落盘,从 files map 派生。
// 单源化 ⇒ 没有 chunks.json 和 files.json 互相不一致的可能。
type JSONStore struct {
	dir string

	mu    sync.RWMutex
	files map[string]*FileMeta    // Path -> FileMeta(磁盘单源)
	byID  map[string]*chunk.Chunk // ID -> Chunk(派生,仅内存)
}

// NewJSONStore 构造一个绑定到指定目录的 JSONStore。目录会在 Load/Flush 时按需创建。
func NewJSONStore(dir string) *JSONStore {
	return &JSONStore{
		dir:   dir,
		files: map[string]*FileMeta{},
		byID:  map[string]*chunk.Chunk{},
	}
}

func (s *JSONStore) filesPath() string { return filepath.Join(s.dir, "files.json") }

// Load 从磁盘读取数据。文件不存在视为空库(首次运行)。
func (s *JSONStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", s.dir, err)
	}

	if data, err := os.ReadFile(s.filesPath()); err == nil {
		var m map[string]*FileMeta
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("store: parse files.json: %w", err)
		}
		s.files = m
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: read files.json: %w", err)
	}

	s.rebuildByID()
	return nil
}

// Flush 原子写回磁盘(先写 .tmp 再 rename,防止崩溃中损坏)。
func (s *JSONStore) Flush() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", s.dir, err)
	}
	return writeJSONAtomic(s.filesPath(), s.files)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: rename %s: %w", path, err)
	}
	return nil
}

// rebuildByID 从 files 重建 ID -> Chunk 反向索引。调用方需要持写锁。
func (s *JSONStore) rebuildByID() {
	s.byID = map[string]*chunk.Chunk{}
	for _, fm := range s.files {
		for _, c := range fm.Chunks {
			if c.FilePath == "" {
				c.FilePath = fm.Path
			}
			s.byID[c.ID] = c
		}
	}
}

// GetChunk 按 ID 查找。
func (s *JSONStore) GetChunk(id string) (*chunk.Chunk, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byID[id]
	return c, ok
}

// AllChunks 返回当前所有 chunk 的浅拷贝切片。
func (s *JSONStore) AllChunks() []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*chunk.Chunk, 0, len(s.byID))
	for _, c := range s.byID {
		out = append(out, c)
	}
	return out
}

// ChunksByFile 按文件路径返回 chunk,源码出现顺序。
func (s *JSONStore) ChunksByFile(path string) []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fm, ok := s.files[path]
	if !ok {
		return nil
	}
	out := make([]*chunk.Chunk, len(fm.Chunks))
	copy(out, fm.Chunks)
	return out
}

// GetFileMeta 返回文件元数据。
func (s *JSONStore) GetFileMeta(path string) (*FileMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.files[path]
	return m, ok
}

// AllFiles 返回所有文件元数据。
func (s *JSONStore) AllFiles() []*FileMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*FileMeta, 0, len(s.files))
	for _, m := range s.files {
		out = append(out, m)
	}
	return out
}

// UpsertFile 是写入核心:一次性替换一个文件的所有 chunk,并尽量复用旧 ID。
//
// ID 复用策略:
//  1. 旧文件的 chunk 按 (Name, Kind) 建成候选表
//  2. 新 chunk 依次去候选表里匹配,命中就继承 ID(并把该候选移除,防止重复继承)
//  3. 未命中的新 chunk 从全局分配新 ID
func (s *JSONStore) UpsertFile(path string, fileHash string, mtime int64, newChunks []*chunk.Chunk) ([]*chunk.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. 收集旧 chunk 作为候选池(key = Name + "|" + Kind)
	type candidate struct {
		id   string
		used bool
	}
	candidates := map[string][]*candidate{}
	oldMeta, hadOld := s.files[path]
	if hadOld {
		for _, oc := range oldMeta.Chunks {
			key := oc.Name + "|" + string(oc.Kind)
			candidates[key] = append(candidates[key], &candidate{id: oc.ID})
		}
	}

	// 2. 构造"全部已存在 ID"集合,用于新 ID 分配时避让冲突
	exists := make(map[string]struct{}, len(s.byID))
	for id := range s.byID {
		exists[id] = struct{}{}
	}

	now := time.Now().Unix()
	resultChunks := make([]*chunk.Chunk, 0, len(newChunks))
	reusedSet := map[string]bool{}

	for _, nc := range newChunks {
		// 确保关键字段齐全
		if nc.FilePath == "" {
			nc.FilePath = path
		}
		nc.UpdatedAt = now

		// 2.1 尝试复用 ID
		key := nc.Name + "|" + string(nc.Kind)
		var reusedID string
		for _, cand := range candidates[key] {
			if !cand.used {
				cand.used = true
				reusedID = cand.id
				break
			}
		}

		if reusedID != "" {
			nc.ID = reusedID
			reusedSet[reusedID] = true
		} else {
			// 2.2 分配新 ID
			newID, err := chunk.AllocateID(exists)
			if err != nil {
				return nil, fmt.Errorf("store: upsert %s: %w", path, err)
			}
			nc.ID = newID
			exists[newID] = struct{}{}
		}

		resultChunks = append(resultChunks, nc)
	}

	// 3. 淘汰:旧文件中未被复用的 chunk 从 byID 摘除
	if hadOld {
		for _, oc := range oldMeta.Chunks {
			if !reusedSet[oc.ID] {
				delete(s.byID, oc.ID)
			}
		}
	}

	// 4. 写入 FileMeta(chunks 直接内嵌,保持源码顺序)
	s.files[path] = &FileMeta{
		Path:    path,
		ModTime: mtime,
		Hash:    fileHash,
		Chunks:  resultChunks,
	}

	// 5. 刷新 byID 中本文件相关条目
	for _, c := range resultChunks {
		s.byID[c.ID] = c
	}

	return resultChunks, nil
}

// DeleteFile 删除文件及其全部 chunk。
func (s *JSONStore) DeleteFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, ok := s.files[path]
	if !ok {
		return nil // 幂等
	}
	for _, c := range meta.Chunks {
		delete(s.byID, c.ID)
	}
	delete(s.files, path)
	return nil
}

// 编译期断言:JSONStore 实现了 ChunkStore。
var _ ChunkStore = (*JSONStore)(nil)
