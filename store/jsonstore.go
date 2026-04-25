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
// 磁盘布局:
//
//	<dir>/
//	  chunks.json    map[ID]Chunk
//	  files.json     map[Path]FileMeta
//
// 反向索引 Name -> []ID 和 Path -> []ID 不落盘,加载时从 chunks 派生。
// 这样数据只有一个真源,不会出现双写不一致。
type JSONStore struct {
	dir string

	mu       sync.RWMutex
	chunks   map[string]*chunk.Chunk // ID -> Chunk
	files    map[string]*FileMeta    // Path -> FileMeta
	byName   map[string][]string     // Name -> []ID (派生,不落盘)
	byFile   map[string][]string     // Path -> []ID (派生,不落盘)
}

// NewJSONStore 构造一个绑定到指定目录的 JSONStore。目录会在 Load/Flush 时按需创建。
func NewJSONStore(dir string) *JSONStore {
	return &JSONStore{
		dir:    dir,
		chunks: map[string]*chunk.Chunk{},
		files:  map[string]*FileMeta{},
		byName: map[string][]string{},
		byFile: map[string][]string{},
	}
}

func (s *JSONStore) chunksPath() string { return filepath.Join(s.dir, "chunks.json") }
func (s *JSONStore) filesPath() string  { return filepath.Join(s.dir, "files.json") }

// Load 从磁盘读取数据。文件不存在视为空库(首次运行)。
func (s *JSONStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", s.dir, err)
	}

	// 读 chunks.json
	if data, err := os.ReadFile(s.chunksPath()); err == nil {
		var m map[string]*chunk.Chunk
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("store: parse chunks.json: %w", err)
		}
		s.chunks = m
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: read chunks.json: %w", err)
	}

	// 读 files.json
	if data, err := os.ReadFile(s.filesPath()); err == nil {
		var m map[string]*FileMeta
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("store: parse files.json: %w", err)
		}
		s.files = m
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: read files.json: %w", err)
	}

	s.rebuildIndexes()
	return nil
}

// Flush 原子写回磁盘(先写 .tmp 再 rename,防止崩溃中损坏)。
func (s *JSONStore) Flush() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", s.dir, err)
	}
	if err := writeJSONAtomic(s.chunksPath(), s.chunks); err != nil {
		return err
	}
	if err := writeJSONAtomic(s.filesPath(), s.files); err != nil {
		return err
	}
	return nil
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

// rebuildIndexes 从主表重建 byName / byFile 反向索引。调用方需要持写锁。
func (s *JSONStore) rebuildIndexes() {
	s.byName = map[string][]string{}
	s.byFile = map[string][]string{}
	for id, c := range s.chunks {
		s.byName[c.Name] = append(s.byName[c.Name], id)
		s.byFile[c.FilePath] = append(s.byFile[c.FilePath], id)
	}
}

// GetChunk 按 ID 查找。
func (s *JSONStore) GetChunk(id string) (*chunk.Chunk, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.chunks[id]
	return c, ok
}

// AllChunks 返回当前所有 chunk 的浅拷贝切片。
func (s *JSONStore) AllChunks() []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*chunk.Chunk, 0, len(s.chunks))
	for _, c := range s.chunks {
		out = append(out, c)
	}
	return out
}

// ChunksByName 按 Name 返回匹配。
func (s *JSONStore) ChunksByName(name string) []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byName[name]
	out := make([]*chunk.Chunk, 0, len(ids))
	for _, id := range ids {
		if c, ok := s.chunks[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

// ChunksByFile 按文件路径返回。
func (s *JSONStore) ChunksByFile(path string) []*chunk.Chunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byFile[path]
	out := make([]*chunk.Chunk, 0, len(ids))
	for _, id := range ids {
		if c, ok := s.chunks[id]; ok {
			out = append(out, c)
		}
	}
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
		for _, oid := range oldMeta.ChunkIDs {
			if oc, ok := s.chunks[oid]; ok {
				key := oc.Name + "|" + string(oc.Kind)
				candidates[key] = append(candidates[key], &candidate{id: oid})
			}
		}
	}

	// 2. 构造"全部已存在 ID"集合,用于新 ID 分配时避让冲突
	exists := make(map[string]struct{}, len(s.chunks))
	for id := range s.chunks {
		exists[id] = struct{}{}
	}

	now := time.Now().Unix()
	finalIDs := make([]string, 0, len(newChunks))
	resultChunks := make([]*chunk.Chunk, 0, len(newChunks))

	for _, nc := range newChunks {
		// 确保关键字段齐全
		if nc.FilePath == "" {
			nc.FilePath = path
		}
		if nc.ContentHash == "" {
			nc.ContentHash = chunk.HashBody(nc.Body)
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
		} else {
			// 2.2 分配新 ID
			newID, err := chunk.AllocateID(exists)
			if err != nil {
				return nil, fmt.Errorf("store: upsert %s: %w", path, err)
			}
			nc.ID = newID
			exists[newID] = struct{}{}
		}

		s.chunks[nc.ID] = nc
		finalIDs = append(finalIDs, nc.ID)
		resultChunks = append(resultChunks, nc)
	}

	// 3. 淘汰:旧文件中未被复用的 chunk
	if hadOld {
		reused := map[string]bool{}
		for _, id := range finalIDs {
			reused[id] = true
		}
		for _, oid := range oldMeta.ChunkIDs {
			if !reused[oid] {
				delete(s.chunks, oid)
			}
		}
	}

	// 4. 更新 FileMeta
	s.files[path] = &FileMeta{
		Path:     path,
		ModTime:  mtime,
		Hash:     fileHash,
		ChunkIDs: finalIDs,
	}

	// 5. 重建受影响的反向索引(简单起见,整个重建;如果成为瓶颈再优化)
	s.rebuildIndexes()

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
	for _, id := range meta.ChunkIDs {
		delete(s.chunks, id)
	}
	delete(s.files, path)
	s.rebuildIndexes()
	return nil
}

// 编译期断言:JSONStore 实现了 ChunkStore。
var _ ChunkStore = (*JSONStore)(nil)
