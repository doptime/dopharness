// Package chunk 定义 AST 切片的核心数据模型与稳定 ID 生成。
//
// 设计要点:
//   - Chunk.ID 是 3 字节的 base64url 编码(4 字符),一旦分配永不变,
//     与文件名/符号名解耦,是系统内部的稳定引用。
//   - Chunk.Name 仅用于人类阅读和模糊定位兜底,不参与 ID 计算。
//   - Chunk.ContentHash 用于增量判定和跨 session 的指纹对齐。
package chunk

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/cespare/xxhash/v2"
)

// Kind 枚举 Chunk 类型。使用字符串而非整型,便于 JSON 存储与人肉排错。
type Kind string

const (
	KindFunction  Kind = "Function"
	KindMethod    Kind = "Method"
	KindStruct    Kind = "Struct"
	KindInterface Kind = "Interface"
	KindClass     Kind = "Class" // TS/JS
	KindType      Kind = "Type"  // 兜底:type alias 等

	// Markdown chunk types(由 parser_md.go 产出)
	KindSection     Kind = "Section"     // ATX 节级标题及其正文
	KindFrontmatter Kind = "Frontmatter" // 文件首部 YAML/TOML 块
	KindPreamble    Kind = "Preamble"    // 第一个标题之前的散文,或无标题文件全文

)

// Chunk 是 AST 切片的通用表达。无论来自 Go、TS、JS 都落到这个结构上。
type Chunk struct {
	// ID 全局稳定唯一标识,格式为 4 字符 base64url(对应 3 字节随机)。
	// 首次入库时由 NewID + 冲突检测分配,此后永不变更,即使文件被重命名。
	ID string `json:"id"`

	// FilePath 是相对项目根的 POSIX 风格路径(正斜杠),例如 "pkg/foo/bar.go"。
	FilePath string `json:"path"`

	// Kind 标注 Chunk 的语义类别。
	Kind Kind `json:"kind"`

	// Name 是人类可读的名字。
	//   - Go  函数: "extractGoFunc"
	//   - Go  方法: "User.Save"
	//   - TS  函数: "parseTSFile"
	//   - TS  方法: "UserService.create"
	// 可能重复,不可用作主键。
	Name string `json:"name"`

	// Skeleton 是签名 + 占位体。用于 Pass1 Triage 时让 LLM 快速扫视。
	Skeleton string `json:"skeleton"`

	// Body 是完整源码(从签名起始到结束的原文切片,保留注释和缩进)。
	Body string `json:"body"`

	// Defines 列出本 Chunk 声明的符号。通常只有 Name 自己。
	Defines []string `json:"defines"`

	// Refs 列出本 Chunk 引用的外部符号。用于符号图构建与粗排序。
	Refs []string `json:"refs,omitempty"`

	// ContentHash 是 Body 的 xxhash64 十六进制。用于:
	//   - 增量索引时判断内容是否真变了(mtime 不可靠)
	//   - 冲突检测:同 ID 不同 hash 说明数据损坏
	ContentHash string `json:"hash"`

	// UpdatedAt 是最后一次入库的 Unix 秒。
	UpdatedAt int64 `json:"updated_at"`
}

// HashBody 计算 body 的内容指纹,供 indexer 决定是否需要重入库。
// 使用 xxhash64 的十六进制形式,16 字符。
func HashBody(body string) string {
	h := xxhash.Sum64String(body)
	return fmt.Sprintf("%016x", h)
}

// NewID 生成一个新的 4 字符 ID(3 字节随机 + base64url 无填充)。
// 注意:这里不做冲突检测,调用方(通常是 store)需要持有现存 ID 集合后决定是否接受。
func NewID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在标准环境下不应失败;如果失败说明系统异常,
		// 此时用 xxhash 兜底仍能产出合法 ID,避免整条流水线中断。
		h := xxhash.Sum64String(fmt.Sprintf("%p-%d", &b, len(b)))
		b[0] = byte(h)
		b[1] = byte(h >> 8)
		b[2] = byte(h >> 16)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// AllocateID 在给定的"已存在 ID 集合"中分配一个不冲突的新 ID。
// 期望冲突率极低:3 字节空间 = 16,777,216,百万级项目仍有充裕余量。
// 最多尝试 64 次,超过则返回错误(说明调用方该考虑扩容到 4 字节了)。
func AllocateID(exists map[string]struct{}) (string, error) {
	for i := 0; i < 64; i++ {
		id := NewID()
		if _, dup := exists[id]; !dup {
			return id, nil
		}
	}
	return "", fmt.Errorf("chunk: failed to allocate unique ID after 64 attempts (existing=%d)", len(exists))
}

// QualifiedName 返回用于跨文件唯一索引的名字形式,格式:"<path>::<name>"。
// 这不是 ID,只是反向索引 Name -> []ID 中的辅助键之一。
func (c *Chunk) QualifiedName() string {
	return c.FilePath + "::" + c.Name
}
