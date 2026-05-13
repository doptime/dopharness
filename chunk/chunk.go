// Package chunk 定义 AST 切片的核心数据模型与稳定 ID 生成。
//
// 设计要点:
//   - Chunk.ID 是 3 字节的 base64url 编码(4 字符),一旦分配永不变,
//     与文件名/符号名解耦,是系统内部的稳定引用。
//   - Chunk.Name 仅用于人类阅读,不参与 ID 计算,也不再做模糊回退。
//   - Chunk 不再保留 Skeleton / Defines / ContentHash:
//       - Skeleton:渲染时按需从 Body 抽首行签名,无需单独字段。
//       - Defines:与 Name 完全冗余。
//       - ContentHash:增量索引用的是文件级 hash(FileMeta.Hash),per-chunk hash 无用。
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

// Chunk 是 AST 切片的通用表达。无论来自 Go、TS、JS、Markdown 都落到这个结构上。
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
	// 可能重复;仅用于展示,不参与定位。
	Name string `json:"name"`

	// Body 是完整源码(从签名起始到结束的原文切片,保留注释和缩进)。
	// 渲染 SKELETON 时按需从 Body 抽首行签名,不再单独存一份。
	Body string `json:"body"`

	// Refs 列出本 Chunk 引用的外部符号。用于符号图构建与粗排序。
	Refs []string `json:"refs,omitempty"`

	// UpdatedAt 是最后一次入库的 Unix 秒。
	UpdatedAt int64 `json:"updated_at"`
}

// HashBody 计算一段文本的内容指纹(xxhash64,16 字符十六进制)。
// 用作文件级 hash(FileMeta.Hash)——per-chunk 已不再 hash。
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

// QualifiedName 返回 "<path>::<name>" 形式,仅用于日志/展示。
func (c *Chunk) QualifiedName() string {
	return c.FilePath + "::" + c.Name
}
