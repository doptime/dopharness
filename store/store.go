// Package store 提供 Chunk 的持久化抽象。
//
// 默认实现是 JSONStore(单目录 + 单个 JSON 文件),满足 SDK 嵌入场景。
// 未来可平替为 Redis / SQLite 而无需改动上层。
package store

import "github.com/doptime/dopharness/chunk"

// FileMeta 记录一个物理文件的索引状态,以及该文件贡献的全部 chunk。
//
// chunk 直接内嵌(而不是用 ID 引用到另一张表)——这样磁盘上只有一份真源,
// 不会出现"chunks.json 和 files.json 互相矛盾"的同步问题。Chunks 内的顺序
// 即源码出现顺序,渲染时直接按此顺序拼出"文件视图"。
type FileMeta struct {
	Path    string         `json:"path"`   // 相对项目根
	ModTime int64          `json:"mtime"`  // 文件最后修改时间(秒)
	Hash    string         `json:"hash"`   // 整文件内容 hash(xxhash64)
	Chunks  []*chunk.Chunk `json:"chunks"` // 该文件的所有 chunk,源码顺序
}

// ChunkStore 是 Chunk 持久化的最小接口。
//
// 实现需要保证:
//   - 单进程内的并发读写安全
//   - ID 分配时的去重(委托给 chunk.AllocateID,需提供 exists 集合)
//   - 文件级的原子替换:UpsertFile 一次性重写一个文件下的所有 chunk
//
// 不再提供 ChunksByName —— LLM 用 ChunkID 定位,没有"按名字回退"的需求。
type ChunkStore interface {
	// Load 从磁盘加载所有数据到内存。首次使用必须调用一次。
	Load() error

	// Flush 将内存状态原子写回磁盘。
	Flush() error

	// GetChunk 按 ID 取 chunk。未找到返回 (nil, false)。
	GetChunk(id string) (*chunk.Chunk, bool)

	// AllChunks 返回当前所有 chunk 的浅拷贝切片(顺序不保证)。
	AllChunks() []*chunk.Chunk

	// ChunksByFile 返回指定文件下的所有 chunk,**按源码顺序**。
	// 用于渲染"文件视图"时按真实位置排列。
	ChunksByFile(path string) []*chunk.Chunk

	// GetFileMeta 返回文件的索引元数据。未记录过返回 (nil, false)。
	GetFileMeta(path string) (*FileMeta, bool)

	// UpsertFile 原子更新一个文件的全部 chunk。
	//
	// 语义:
	//   - 复用 ID:newChunks 中若 Name 与旧记录匹配,则继承旧 ID。
	//   - 新分配:未匹配的 chunk 分配新 ID。
	//   - 淘汰:旧记录中未出现于 newChunks 的 chunk 会被删除。
	//
	// 返回"最终入库"的 chunk 列表(已填好 ID、UpdatedAt)。
	UpsertFile(path string, fileHash string, mtime int64, newChunks []*chunk.Chunk) ([]*chunk.Chunk, error)

	// DeleteFile 删除一个文件及其所有 chunk 索引。
	// 用于用户删除了源文件后清理僵尸记录。
	DeleteFile(path string) error

	// AllFiles 返回所有已索引文件的元数据切片。
	AllFiles() []*FileMeta
}
