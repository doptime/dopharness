package edit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// Action 是修改类型。
type Action string

const (
	// ActionModify 替换一个已存在 chunk 的内容。
	// 必填:ChunkID 或 Name(二选一,ChunkID 优先);NewContent。
	ActionModify Action = "MODIFY"

	// ActionDeleteChunk 删除一个已存在的 chunk。
	// 必填:ChunkID 或 Name。
	ActionDeleteChunk Action = "DELETE_CHUNK"

	// ActionAddChunk 在已存在文件的末尾追加一个新 chunk。
	// 必填:FilePath、NewContent。
	ActionAddChunk Action = "ADD_CHUNK"

	// ActionCreateFile 创建一个不存在的文件。
	// 必填:FilePath、NewContent。
	// 如果文件已存在,会拒绝(要求改用 MODIFY / ADD_CHUNK)。
	ActionCreateFile Action = "CREATE_FILE"

	// ActionDeleteFile 删除整个文件(及其所有 chunk)。
	// 必填:FilePath。
	ActionDeleteFile Action = "DELETE_FILE"
)

// Modification 是 LLM 通过 ToolCall 提交的单个变更请求。
type Modification struct {
	Action Action `json:"action"`

	// ChunkID 目标 chunk 的稳定 ID。
	// MODIFY/DELETE_CHUNK 时必须有 ChunkID 或 Name 之一。
	ChunkID string `json:"chunk_id,omitempty"`

	// Name 是 LLM 可能写错 ID 时的兜底名称(如 "User.Save")。
	Name string `json:"name,omitempty"`

	// FilePath 是相对项目根的路径,用 POSIX 正斜杠。
	// CREATE_FILE/DELETE_FILE/ADD_CHUNK 时必填。
	FilePath string `json:"file_path,omitempty"`

	// NewContent 是新代码的完整文本。MODIFY/ADD_CHUNK/CREATE_FILE 时必填。
	// 对 MODIFY 来说,应当是完整的 AST 顶层声明(含签名和 body)。
	NewContent string `json:"new_content,omitempty"`
}

// ApplyOutcome 描述单个修改的执行结果。
type ApplyOutcome string

const (
	OutcomeApplied    ApplyOutcome = "applied"    // 成功应用,已落盘
	OutcomeNoOp       ApplyOutcome = "noop"       // 幂等跳过(如 DELETE 一个不存在的文件)
	OutcomeValidation ApplyOutcome = "validation" // 语法校验失败(文件未变更)
	OutcomeLocation   ApplyOutcome = "location"   // 定位失败(LLM 说的 ID/Name 找不到或有歧义)
	OutcomeConflict   ApplyOutcome = "conflict"   // 前置条件冲突(如 CREATE_FILE 但文件已存在)
	OutcomeIOError    ApplyOutcome = "io_error"   // 磁盘读写失败
)

// ApplyResult 是一次 Apply 调用的结果。
type ApplyResult struct {
	Modification *Modification // 原始请求,方便日志定位
	Outcome      ApplyOutcome
	Message      string      // 人类 / LLM 可读的详细说明
	AffectedIDs  []string    // 受影响的 chunk ID(新建、复用、删除的都列出)
	Validation   *ValidationError // Outcome == OutcomeValidation 时非 nil
}

// Error 返回一个可作为 tool 返回值传回给 LLM 的错误;如果 Outcome 是成功/noop 则返回 nil。
func (r *ApplyResult) Error() error {
	switch r.Outcome {
	case OutcomeApplied, OutcomeNoOp:
		return nil
	}
	return errors.New(r.Message)
}

// Applier 是修改应用器。持有 store 和校验器,自带每文件锁防止并发写冲突。
type Applier struct {
	// ProjectRoot 是项目根绝对路径。所有 FilePath 基于它解析。
	ProjectRoot string

	// Store 是 chunk 存储。
	Store store.ChunkStore

	// Locator 用于 MODIFY/DELETE_CHUNK 定位 chunk。
	Locator *Locator

	// Validator 做两级语法校验。
	Validator *Validator

	// GoParse 是解析 Go 文件的函数。注入而非硬依赖,便于单测。
	GoParse func(absPath, relPath string) ([]*chunk.Chunk, error)

	// TSParse 是批量解析 TS 文件的函数。注入。可以为 nil(则 TS 文件修改不会更新索引)。
	TSParse func(absPath, relPath string) ([]*chunk.Chunk, error)

	// fileLocks 给每个文件一把独立的锁,防止两个并发修改同一个文件相互覆盖。
	fileLocksMu sync.Mutex
	fileLocks   map[string]*sync.Mutex
}

// NewApplier 构造。
func NewApplier(projectRoot string, s store.ChunkStore, v *Validator) *Applier {
	return &Applier{
		ProjectRoot: projectRoot,
		Store:       s,
		Locator:     NewLocator(s),
		Validator:   v,
		fileLocks:   map[string]*sync.Mutex{},
	}
}

// lockFile 获取/创建某文件的锁,保证同一 Applier 上对同文件的并发修改串行化。
func (a *Applier) lockFile(relPath string) *sync.Mutex {
	a.fileLocksMu.Lock()
	defer a.fileLocksMu.Unlock()
	if m, ok := a.fileLocks[relPath]; ok {
		return m
	}
	m := &sync.Mutex{}
	a.fileLocks[relPath] = m
	return m
}

// Apply 执行单个修改。
// 这是主入口。返回值一定非 nil;失败情况通过 Outcome 区分,error 通过 result.Error() 获取。
func (a *Applier) Apply(mod *Modification) *ApplyResult {
	res := &ApplyResult{Modification: mod}

	// 前置:action 正当性检查
	switch mod.Action {
	case ActionModify, ActionDeleteChunk, ActionAddChunk, ActionCreateFile, ActionDeleteFile:
		// ok
	default:
		res.Outcome = OutcomeConflict
		res.Message = fmt.Sprintf("unknown action %q", mod.Action)
		return res
	}

	switch mod.Action {
	case ActionModify:
		return a.applyModify(mod, res)
	case ActionDeleteChunk:
		return a.applyDeleteChunk(mod, res)
	case ActionAddChunk:
		return a.applyAddChunk(mod, res)
	case ActionCreateFile:
		return a.applyCreateFile(mod, res)
	case ActionDeleteFile:
		return a.applyDeleteFile(mod, res)
	}
	return res // unreachable
}

// ApplyBatch 批量执行,彼此独立;一个失败不影响其他。
// 返回值和输入一一对应。
func (a *Applier) ApplyBatch(mods []*Modification) []*ApplyResult {
	out := make([]*ApplyResult, len(mods))
	for i, m := range mods {
		out[i] = a.Apply(m)
	}
	return out
}

// ---- 以下是各 Action 的具体实现 ----

// applyModify 替换一个已存在 chunk 的内容。
//
// 流程:
//  1. Locator 定位目标 chunk
//  2. 读取原文件(Body 外的上下文都要保留)
//  3. 用 NewContent 替换 chunk 对应的字节区间
//  4. 两级语法校验
//  5. 写盘 + 重新解析整个文件 + 更新 store
//  6. 任何失败都回滚文件到原始内容
func (a *Applier) applyModify(mod *Modification, res *ApplyResult) *ApplyResult {
	if mod.NewContent == "" {
		res.Outcome = OutcomeConflict
		res.Message = "MODIFY requires NewContent"
		return res
	}
	loc := a.Locator.Locate(mod.ChunkID, mod.Name)
	switch loc.Outcome {
	case LocateMissing:
		res.Outcome = OutcomeLocation
		res.Message = loc.Message
		return res
	case LocateAmbiguous:
		res.Outcome = OutcomeLocation
		res.Message = loc.Message
		return res
	}
	target := loc.Chunk

	// 文件级锁
	unlock := a.acquire(target.FilePath)
	defer unlock()

	absPath := a.absPath(target.FilePath)
	contentBefore, err := os.ReadFile(absPath)
	if err != nil {
		res.Outcome = OutcomeIOError
		res.Message = fmt.Sprintf("read %s: %v", target.FilePath, err)
		return res
	}

	// 在原文件中定位 Body 字符串区间(用字节匹配;Body 来自最新 parser 产出,应仍在文件中)
	start, end, ok := findBodyRange(contentBefore, target.Body)
	if !ok {
		// 说明 store 里的 Body 和磁盘不同步(可能是在上次索引后有外部修改)
		// 告知 LLM 重建索引
		res.Outcome = OutcomeConflict
		res.Message = fmt.Sprintf(
			"chunk %s body no longer matches disk content in %s — file was modified outside dopharness. Re-run indexing.",
			target.ID, target.FilePath)
		return res
	}

	// 合并新内容
	merged := assembleMerged(contentBefore, start, end, mod.NewContent)

	// 校验
	if vErr := a.Validator.Validate(target.FilePath, mod.NewContent, string(merged)); vErr != nil {
		res.Outcome = OutcomeValidation
		res.Message = vErr.Error()
		res.Validation = vErr
		return res
	}

	// 写盘 + 重新索引该文件(用"先写再 parser"保证 store 反映真实)
	if err := a.writeAndReindex(target.FilePath, absPath, merged, contentBefore); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}

	res.Outcome = OutcomeApplied
	res.Message = fmt.Sprintf("modified chunk %s in %s", target.ID, target.FilePath)
	res.AffectedIDs = []string{target.ID}
	if loc.Outcome == LocateFuzzyUnique {
		res.Message += " (warning: " + loc.Message + ")"
	}
	return res
}

// applyDeleteChunk 从文件里移除一个 chunk(删字节,不影响其他 chunk)。
func (a *Applier) applyDeleteChunk(mod *Modification, res *ApplyResult) *ApplyResult {
	loc := a.Locator.Locate(mod.ChunkID, mod.Name)
	switch loc.Outcome {
	case LocateMissing:
		res.Outcome = OutcomeLocation
		res.Message = loc.Message
		return res
	case LocateAmbiguous:
		res.Outcome = OutcomeLocation
		res.Message = loc.Message
		return res
	}
	target := loc.Chunk
	unlock := a.acquire(target.FilePath)
	defer unlock()

	absPath := a.absPath(target.FilePath)
	contentBefore, err := os.ReadFile(absPath)
	if err != nil {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}
	start, end, ok := findBodyRange(contentBefore, target.Body)
	if !ok {
		res.Outcome = OutcomeConflict
		res.Message = fmt.Sprintf("chunk body not found on disk for %s", target.FilePath)
		return res
	}

	// 删除 body 并尽量清理前后多余空行(至多保留一个空行分隔)
	merged := deleteRangeNormalizingBlankLines(contentBefore, start, end)

	// 删除通常不需要单片校验(没有片段),只做合并后校验
	if vErr := a.Validator.Validate(target.FilePath, "", string(merged)); vErr != nil {
		res.Outcome = OutcomeValidation
		res.Message = vErr.Error()
		res.Validation = vErr
		return res
	}

	if err := a.writeAndReindex(target.FilePath, absPath, merged, contentBefore); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}
	res.Outcome = OutcomeApplied
	res.Message = fmt.Sprintf("deleted chunk %s from %s", target.ID, target.FilePath)
	res.AffectedIDs = []string{target.ID}
	return res
}

// applyAddChunk 在已存在的文件末尾追加新 chunk。
func (a *Applier) applyAddChunk(mod *Modification, res *ApplyResult) *ApplyResult {
	if mod.FilePath == "" || mod.NewContent == "" {
		res.Outcome = OutcomeConflict
		res.Message = "ADD_CHUNK requires FilePath and NewContent"
		return res
	}
	rel := normalizeRelPath(mod.FilePath)
	absPath := a.absPath(rel)
	unlock := a.acquire(rel)
	defer unlock()

	contentBefore, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			res.Outcome = OutcomeConflict
			res.Message = fmt.Sprintf("file %s does not exist; use CREATE_FILE instead", rel)
			return res
		}
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}

	// 拼接:保证文件末尾有恰好 2 个换行分隔,然后加 NewContent,再补 1 个末尾换行
	merged := append([]byte{}, contentBefore...)
	merged = ensureTrailingSep(merged)
	merged = append(merged, []byte(mod.NewContent)...)
	if len(merged) == 0 || merged[len(merged)-1] != '\n' {
		merged = append(merged, '\n')
	}

	if vErr := a.Validator.Validate(rel, mod.NewContent, string(merged)); vErr != nil {
		res.Outcome = OutcomeValidation
		res.Message = vErr.Error()
		res.Validation = vErr
		return res
	}

	if err := a.writeAndReindex(rel, absPath, merged, contentBefore); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}

	// 算出新增了哪些 chunk ID:新索引 - 原索引
	newIDs := diffAddedIDs(a.Store, rel, contentBefore)
	res.Outcome = OutcomeApplied
	res.Message = fmt.Sprintf("added to %s", rel)
	res.AffectedIDs = newIDs
	return res
}

// applyCreateFile 创建一个不存在的文件。
func (a *Applier) applyCreateFile(mod *Modification, res *ApplyResult) *ApplyResult {
	if mod.FilePath == "" || mod.NewContent == "" {
		res.Outcome = OutcomeConflict
		res.Message = "CREATE_FILE requires FilePath and NewContent"
		return res
	}
	rel := normalizeRelPath(mod.FilePath)
	if !isSupportedExtension(rel) {
		res.Outcome = OutcomeConflict
		res.Message = fmt.Sprintf("unsupported extension for %s (supported: .go .ts .tsx .js .jsx)", rel)
		return res
	}
	absPath := a.absPath(rel)
	unlock := a.acquire(rel)
	defer unlock()

	if _, err := os.Stat(absPath); err == nil {
		res.Outcome = OutcomeConflict
		res.Message = fmt.Sprintf("file %s already exists; use MODIFY or ADD_CHUNK", rel)
		return res
	} else if !os.IsNotExist(err) {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}

	// 对新建文件:snippet 就是 NewContent,merged 也是它(没有前后文)
	if vErr := a.Validator.Validate(rel, mod.NewContent, mod.NewContent); vErr != nil {
		res.Outcome = OutcomeValidation
		res.Message = vErr.Error()
		res.Validation = vErr
		return res
	}

	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = fmt.Sprintf("mkdir: %v", err)
		return res
	}

	// 无需回滚(文件原本不存在,写失败时直接删)
	content := []byte(mod.NewContent)
	if len(content) == 0 || content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}
	if err := writeFileAtomic(absPath, content); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}

	// 索引新文件
	if err := a.reindexFile(rel, absPath); err != nil {
		// 写成功但索引失败:记为部分成功,LLM 可以下次重建索引恢复
		res.Outcome = OutcomeApplied
		res.Message = fmt.Sprintf("file created but index update failed: %v", err)
	} else {
		res.Outcome = OutcomeApplied
		res.Message = fmt.Sprintf("created %s", rel)
	}
	for _, c := range a.Store.ChunksByFile(rel) {
		res.AffectedIDs = append(res.AffectedIDs, c.ID)
	}
	return res
}

// applyDeleteFile 删除整个文件。
func (a *Applier) applyDeleteFile(mod *Modification, res *ApplyResult) *ApplyResult {
	if mod.FilePath == "" {
		res.Outcome = OutcomeConflict
		res.Message = "DELETE_FILE requires FilePath"
		return res
	}
	rel := normalizeRelPath(mod.FilePath)
	absPath := a.absPath(rel)
	unlock := a.acquire(rel)
	defer unlock()

	// 先记录要删的 chunk ID,方便返回给 LLM
	for _, c := range a.Store.ChunksByFile(rel) {
		res.AffectedIDs = append(res.AffectedIDs, c.ID)
	}

	if err := os.Remove(absPath); err != nil {
		if os.IsNotExist(err) {
			// 幂等:文件不存在时,只要 store 里也清了就算 noop
			_ = a.Store.DeleteFile(rel)
			res.Outcome = OutcomeNoOp
			res.Message = fmt.Sprintf("file %s did not exist", rel)
			return res
		}
		res.Outcome = OutcomeIOError
		res.Message = err.Error()
		return res
	}
	if err := a.Store.DeleteFile(rel); err != nil {
		res.Outcome = OutcomeIOError
		res.Message = fmt.Sprintf("file removed but store delete failed: %v", err)
		return res
	}
	res.Outcome = OutcomeApplied
	res.Message = fmt.Sprintf("deleted %s", rel)
	return res
}

// ---- 辅助函数 ----

// absPath 把相对路径转为绝对路径。
func (a *Applier) absPath(rel string) string {
	return filepath.Join(a.ProjectRoot, filepath.FromSlash(rel))
}

// acquire 获取文件锁,返回 unlock 函数。
func (a *Applier) acquire(relPath string) func() {
	m := a.lockFile(relPath)
	m.Lock()
	return m.Unlock
}

// writeAndReindex 原子写盘 + 重新索引。写失败或索引失败都会回滚文件。
func (a *Applier) writeAndReindex(rel, absPath string, newContent, oldContent []byte) error {
	if err := writeFileAtomic(absPath, newContent); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := a.reindexFile(rel, absPath); err != nil {
		// 回滚磁盘
		if rbErr := writeFileAtomic(absPath, oldContent); rbErr != nil {
			return fmt.Errorf("reindex %s failed (%v) AND rollback failed (%v)", rel, err, rbErr)
		}
		return fmt.Errorf("reindex %s failed: %w (rolled back)", rel, err)
	}
	return nil
}

// reindexFile 用对应语言的 parser 重新解析一个文件,并写入 store。
// 这是"写盘后的真相核对"—— LLM 产出的代码落盘后,必须重新 parser 才知道里面到底有哪些 chunk。
func (a *Applier) reindexFile(rel, absPath string) error {
	ext := strings.ToLower(filepath.Ext(rel))
	var (
		chunks []*chunk.Chunk
		err    error
	)
	switch ext {
	case ".go":
		if a.GoParse == nil {
			return errors.New("GoParse not configured")
		}
		chunks, err = a.GoParse(absPath, rel)
	case ".ts", ".tsx", ".js", ".jsx":
		if a.TSParse == nil {
			return errors.New("TSParse not configured")
		}
		chunks, err = a.TSParse(absPath, rel)
	default:
		return fmt.Errorf("unsupported extension %s", ext)
	}
	if err != nil {
		return err
	}

	// 计算文件 hash 和 mtime 以更新 FileMeta
	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	stat, err := os.Stat(absPath)
	if err != nil {
		return err
	}
	hash := chunk.HashBody(string(content))
	_, err = a.Store.UpsertFile(rel, hash, stat.ModTime().Unix(), chunks)
	return err
}

// writeFileAtomic:先写 .tmp 再 rename,防止中途崩溃留下半截文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// findBodyRange 在 content 中找到 body 的字节区间 [start, end)。
// 为了稳健,尝试精确匹配;如果失败再尝试去掉首尾空白再找。
// 未找到返回 ok=false。
func findBodyRange(content []byte, body string) (int, int, bool) {
	b := []byte(body)
	if idx := indexBytes(content, b); idx >= 0 {
		return idx, idx + len(b), true
	}
	// 宽松匹配:尝试去除 body 两端空白
	trimmed := strings.TrimSpace(body)
	if trimmed != body && trimmed != "" {
		tb := []byte(trimmed)
		if idx := indexBytes(content, tb); idx >= 0 {
			return idx, idx + len(tb), true
		}
	}
	return -1, -1, false
}

// indexBytes:bytes.Index 的别名封装,方便在测试里替换。
func indexBytes(haystack, needle []byte) int {
	return indexOfBytes(haystack, needle)
}

// indexOfBytes 是 bytes.Index 的内联实现,避免再 import "bytes"(已 import)。
func indexOfBytes(h, n []byte) int {
	if len(n) == 0 {
		return 0
	}
	if len(n) > len(h) {
		return -1
	}
	// 用标准库算法会更快,但这里规模小,直接朴素
	last := len(h) - len(n)
	for i := 0; i <= last; i++ {
		match := true
		for j := 0; j < len(n); j++ {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// assembleMerged 把 content[0:start] + newContent + content[end:] 拼起来。
func assembleMerged(content []byte, start, end int, newContent string) []byte {
	out := make([]byte, 0, len(content)-(end-start)+len(newContent))
	out = append(out, content[:start]...)
	out = append(out, []byte(newContent)...)
	out = append(out, content[end:]...)
	return out
}

// deleteRangeNormalizingBlankLines 删除 [start, end) 并把前后多余空行压缩成一个。
func deleteRangeNormalizingBlankLines(content []byte, start, end int) []byte {
	// 向前吃掉紧邻的换行符(最多一个 \n,保留一个分隔)
	for start > 0 && content[start-1] == '\n' && (start < 2 || content[start-2] == '\n') {
		start--
	}
	// 向后吃掉紧邻的换行
	for end < len(content) && content[end] == '\n' && (end+1 >= len(content) || content[end+1] == '\n') {
		end++
	}
	out := make([]byte, 0, len(content)-(end-start))
	out = append(out, content[:start]...)
	out = append(out, content[end:]...)
	return out
}

// ensureTrailingSep 保证字节 slice 以至少一个空行结尾(便于追加新顶层声明)。
func ensureTrailingSep(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	// 规范化:去掉末尾所有换行,再加 "\n\n"
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return append(b, '\n', '\n')
}

// diffAddedIDs:调用 reindex 后,返回"现在有但原 content 时没有"的 chunk ID 集合。
// 实现用近似:统计 reindex 后的 IDs 中 UpdatedAt == 最新 的。
// 更精确的做法要求 UpsertFile 返回新增列表,当前未做。
// 这里简化为:返回文件里所有 chunk(调用方通常不关心精确集合)。
func diffAddedIDs(s store.ChunkStore, rel string, _ []byte) []string {
	cs := s.ChunksByFile(rel)
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

// normalizeRelPath 把用户可能传入的各种路径格式统一成正斜杠、无前导 "./"。
func normalizeRelPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return p
}

// isSupportedExtension 判断文件名是否是我们支持的语言。
func isSupportedExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx":
		return true
	}
	return false
}
