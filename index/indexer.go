// Package index 是 dopharness 的索引构建层。
//
// 职责:
//   - 扫描项目目录,筛选支持的源码文件(.go / .ts / .tsx / .jsx)
//   - 增量判定:mtime 一级过滤 + 内容 hash 二级过滤
//   - 调度 Go parser (进程内,per-file 并发) 和 TS parser (跨进程,批量单次)
//   - 汇总结果写入 ChunkStore
//   - 删除检测:磁盘上不存在的文件,从 store 清除
//
// 不做的事:
//   - 不做 LLM 调用(那是 gateway 层的职责)
//   - 不做修改回写(edit 层的职责)
package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/doptime/dopharness/chunk"
	"github.com/doptime/dopharness/store"
)

// Config 控制索引行为。
type Config struct {
	// ProjectRoot 是项目根目录的绝对路径。所有 Chunk.FilePath 将相对于它。
	ProjectRoot string

	// Store 是 Chunk 持久化后端。Indexer 不负责 Load/Flush,调用方自己管理时机。
	Store store.ChunkStore

	// TSParser 提供 TS/JS 解析能力。可以为 nil(此时遇到 .ts/.tsx/.jsx 文件会记录错误但不中断)。
	TSParser *chunk.TSParser

	// GoConcurrency 是 Go 文件解析的并发度。0 或负数使用 runtime.NumCPU()。
	GoConcurrency int

	// IgnoreDirs 额外忽略的目录名(除默认的 .git / node_modules / vendor / .dopharness 外)。
	IgnoreDirs []string

	// Logger 如果设置,每处理一个文件调用一次。用于进度反馈。可以为 nil。
	Logger func(event string, path string, extra map[string]any)
}

// Indexer 是索引构建器。单实例可复用多次 Run 调用。
type Indexer struct {
	cfg Config

	// 内部运行时状态:每次 Run 重置
	ignoreSet map[string]struct{}
}

// NewIndexer 构造一个 Indexer。
func NewIndexer(cfg Config) (*Indexer, error) {
	if cfg.ProjectRoot == "" {
		return nil, errors.New("index: ProjectRoot is required")
	}
	abs, err := filepath.Abs(cfg.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("index: resolve ProjectRoot: %w", err)
	}
	cfg.ProjectRoot = abs

	if cfg.Store == nil {
		return nil, errors.New("index: Store is required")
	}
	if cfg.GoConcurrency <= 0 {
		cfg.GoConcurrency = runtime.NumCPU()
	}

	// 默认忽略集合 + 用户补充
	ignore := map[string]struct{}{
		".git":         {},
		".svn":         {},
		".hg":          {},
		"node_modules": {},
		"vendor":       {},
		".dopharness":  {},
		"dist":         {},
		"build":        {},
		".next":        {},
	}
	for _, d := range cfg.IgnoreDirs {
		ignore[d] = struct{}{}
	}

	return &Indexer{cfg: cfg, ignoreSet: ignore}, nil
}

// Report 汇总一次索引运行的结果。
type Report struct {
	FilesScanned   int      // 目录遍历发现的支持文件总数
	FilesSkipped   int      // mtime+hash 命中被跳过的
	FilesIndexed   int      // 真正解析入库的
	FilesFailed    int      // 解析失败的
	FilesRemoved   int      // 在磁盘上不存在了、从 store 中清除的
	ChunksTotal    int      // 入库后的 chunk 总数(所有文件加总)
	Errors         []error  // 按顺序汇总的错误(单文件失败不阻断)
}

func (r *Report) addErr(err error) {
	if err != nil {
		r.Errors = append(r.Errors, err)
	}
}

// Run 执行一次完整的索引。
// 可通过 ctx 取消(粗粒度:在每个文件处理前检查)。
func (ix *Indexer) Run(ctx context.Context) (*Report, error) {
	rep := &Report{}

	// 1. 扫描目录,得到所有候选文件(绝对路径)
	goFiles, tsFiles, err := ix.scan(ctx)
	if err != nil {
		return rep, fmt.Errorf("index: scan: %w", err)
	}
	rep.FilesScanned = len(goFiles) + len(tsFiles)

	// 2. 增量判定:对每个候选文件,决定是"跳过"、"解析"还是"跳过但更新 mtime"
	//    为了让 hash 检查只读一次,我们在过滤阶段就把 body + hash 缓存下来
	goTasks, err := ix.filterIncremental(ctx, goFiles, &rep.FilesSkipped)
	if err != nil {
		return rep, err
	}
	tsTasks, err := ix.filterIncremental(ctx, tsFiles, &rep.FilesSkipped)
	if err != nil {
		return rep, err
	}

	// 3. 分两路并行解析:Go 走 per-file 协程池,TS 走单次批量
	var wg sync.WaitGroup

	var goResults []*parseResult
	var tsResults []*parseResult
	var goMu, tsMu sync.Mutex

	if len(goTasks) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := ix.parseGoFiles(ctx, goTasks)
			goMu.Lock()
			goResults = res
			goMu.Unlock()
		}()
	}
	if len(tsTasks) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := ix.parseTSFiles(ctx, tsTasks)
			tsMu.Lock()
			tsResults = res
			tsMu.Unlock()
		}()
	}
	wg.Wait()

	// 4. 入库(串行,避免 store 内部锁开销)
	for _, r := range append(goResults, tsResults...) {
		if r.err != nil {
			rep.FilesFailed++
			rep.addErr(fmt.Errorf("parse %s: %w", r.task.relPath, r.err))
			ix.log("parse_failed", r.task.relPath, map[string]any{"error": r.err.Error()})
			continue
		}
		if _, err := ix.cfg.Store.UpsertFile(r.task.relPath, r.task.fileHash, r.task.mtime, r.chunks); err != nil {
			rep.FilesFailed++
			rep.addErr(fmt.Errorf("upsert %s: %w", r.task.relPath, err))
			ix.log("upsert_failed", r.task.relPath, map[string]any{"error": err.Error()})
			continue
		}
		rep.FilesIndexed++
		ix.log("indexed", r.task.relPath, map[string]any{"chunks": len(r.chunks)})
	}

	// 5. 删除检测:从 store 中清除在磁盘上已不存在的文件
	removed, err := ix.pruneDeleted(goFiles, tsFiles)
	if err != nil {
		rep.addErr(err)
	}
	rep.FilesRemoved = removed

	// 6. 统计最终 chunk 数
	rep.ChunksTotal = len(ix.cfg.Store.AllChunks())

	return rep, nil
}

// log 是内部日志的薄封装。
func (ix *Indexer) log(event, path string, extra map[string]any) {
	if ix.cfg.Logger != nil {
		ix.cfg.Logger(event, path, extra)
	}
}

// scan 遍历 ProjectRoot,返回所有支持文件的绝对路径,分 Go 和 TS 两组。
func (ix *Indexer) scan(ctx context.Context) (goFiles, tsFiles []string, err error) {
	walkErr := filepath.WalkDir(ix.cfg.ProjectRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// 单个条目读失败不中断整个扫描;记一笔继续
			ix.log("walk_error", path, map[string]any{"error": walkErr.Error()})
			return nil
		}
		// 可取消
		if err := ctx.Err(); err != nil {
			return err
		}

		if d.IsDir() {
			name := d.Name()
			if path == ix.cfg.ProjectRoot {
				return nil // 根目录本身不跳
			}
			if _, skip := ix.ignoreSet[name]; skip {
				return filepath.SkipDir
			}
			// 隐藏目录(以 . 开头)除根目录外一律跳
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go":
			goFiles = append(goFiles, path)
		case ".ts", ".tsx", ".jsx":
			tsFiles = append(tsFiles, path)
		case ".js":
			// .js 也由 TS sidecar 处理(Bun 兼容 JS)
			tsFiles = append(tsFiles, path)
		}
		return nil
	})
	return goFiles, tsFiles, walkErr
}

// parseTask 是一个"已确认需要解析"的文件,携带它的 hash 和 mtime。
type parseTask struct {
	absPath  string
	relPath  string
	mtime    int64
	fileHash string
	content  []byte // 预读的文件内容,避免 parser 再读一遍
}

// filterIncremental 对候选文件做增量过滤。
//
// 规则:
//  1. 磁盘 mtime == store 中记录的 mtime  →  跳过(假定未改)
//  2. 否则读文件,算 hash:
//     a. hash == store 中记录的 hash  →  跳过,但更新 mtime(对抗 git checkout)
//     b. hash 不同或文件未索引过  →  纳入 parseTask 列表
func (ix *Indexer) filterIncremental(ctx context.Context, files []string, skippedCounter *int) ([]*parseTask, error) {
	tasks := make([]*parseTask, 0, len(files))
	for _, abs := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		rel, err := relPath(ix.cfg.ProjectRoot, abs)
		if err != nil {
			ix.log("relpath_error", abs, map[string]any{"error": err.Error()})
			continue
		}

		stat, err := os.Stat(abs)
		if err != nil {
			ix.log("stat_error", abs, map[string]any{"error": err.Error()})
			continue
		}
		mtime := stat.ModTime().Unix()

		meta, hadMeta := ix.cfg.Store.GetFileMeta(rel)
		// 第一级过滤:mtime
		if hadMeta && meta.ModTime == mtime {
			*skippedCounter++
			continue
		}

		// 读内容,算 hash
		content, err := os.ReadFile(abs)
		if err != nil {
			ix.log("read_error", abs, map[string]any{"error": err.Error()})
			continue
		}
		hash := fmt.Sprintf("%016x", xxhash.Sum64(content))

		// 第二级过滤:hash
		if hadMeta && meta.Hash == hash {
			// 内容没变,但 mtime 变了(典型:git checkout)。
			// 我们还是更新一下 store 里的 mtime,避免下次还走到这里白读一次文件。
			// 做法:构造一个"空新 chunks"的 upsert 会丢数据,所以直接用一个零 chunk 的 upsert 不可行。
			// 退化方案:把现有 chunk 重新喂给 UpsertFile(它会复用 ID),代价是一次拷贝,可接受。
			existing := ix.cfg.Store.ChunksByFile(rel)
			if len(existing) > 0 {
				copied := make([]*chunk.Chunk, 0, len(existing))
				for _, c := range existing {
					// 浅拷贝足矣,UpsertFile 会更新 UpdatedAt 和 hash
					cc := *c
					copied = append(copied, &cc)
				}
				_, _ = ix.cfg.Store.UpsertFile(rel, hash, mtime, copied)
			}
			*skippedCounter++
			continue
		}

		tasks = append(tasks, &parseTask{
			absPath:  abs,
			relPath:  rel,
			mtime:    mtime,
			fileHash: hash,
			content:  content,
		})
	}
	return tasks, nil
}

// parseResult 是 parseGo/parseTS 内部的中间结果。
type parseResult struct {
	task   *parseTask
	chunks []*chunk.Chunk
	err    error
}

// parseGoFiles 并发解析 Go 文件。
func (ix *Indexer) parseGoFiles(ctx context.Context, tasks []*parseTask) []*parseResult {
	results := make([]*parseResult, len(tasks))
	sem := make(chan struct{}, ix.cfg.GoConcurrency)
	var wg sync.WaitGroup

	for i, task := range tasks {
		if err := ctx.Err(); err != nil {
			// 剩余任务不启动,直接记 err
			for j := i; j < len(tasks); j++ {
				results[j] = &parseResult{task: tasks[j], err: err}
			}
			break
		}

		wg.Add(1)
		go func(idx int, t *parseTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			chunks, err := chunk.ParseGoFile(t.absPath, t.relPath)
			results[idx] = &parseResult{task: t, chunks: chunks, err: err}
		}(i, task)
	}
	wg.Wait()
	return results
}

// parseTSFiles 批量解析 TS 文件,一次 Bun 启动。
func (ix *Indexer) parseTSFiles(ctx context.Context, tasks []*parseTask) []*parseResult {
	_ = ctx // 当前 TS parser 的 exec 不支持取消;未来可升级

	if ix.cfg.TSParser == nil {
		results := make([]*parseResult, len(tasks))
		for i, t := range tasks {
			results[i] = &parseResult{
				task: t,
				err:  errors.New("index: TSParser not configured, cannot parse .ts/.tsx/.jsx/.js"),
			}
		}
		return results
	}

	reqs := make([]chunk.TSFileReq, len(tasks))
	byAbs := make(map[string]*parseTask, len(tasks))
	for i, t := range tasks {
		reqs[i] = chunk.TSFileReq{AbsPath: t.absPath, RelPath: t.relPath}
		byAbs[t.absPath] = t
	}

	tsResults, err := ix.cfg.TSParser.ParseTSFiles(reqs)
	if err != nil {
		// 整批失败:所有任务标错
		out := make([]*parseResult, len(tasks))
		for i, t := range tasks {
			out[i] = &parseResult{task: t, err: fmt.Errorf("ts batch: %w", err)}
		}
		return out
	}

	out := make([]*parseResult, 0, len(tasks))
	for _, r := range tsResults {
		t := byAbs[r.AbsPath]
		if t == nil {
			// sidecar 返回了不在请求里的路径?忽略
			continue
		}
		out = append(out, &parseResult{
			task:   t,
			chunks: r.Chunks,
			err:    r.Err,
		})
	}
	return out
}

// pruneDeleted 删除 store 中存在、但磁盘上已不见的文件条目。
// 参数是本次扫描到的磁盘上文件的绝对路径集合(Go + TS 合并)。
func (ix *Indexer) pruneDeleted(goFiles, tsFiles []string) (int, error) {
	onDisk := make(map[string]struct{}, len(goFiles)+len(tsFiles))
	for _, abs := range goFiles {
		if rel, err := relPath(ix.cfg.ProjectRoot, abs); err == nil {
			onDisk[rel] = struct{}{}
		}
	}
	for _, abs := range tsFiles {
		if rel, err := relPath(ix.cfg.ProjectRoot, abs); err == nil {
			onDisk[rel] = struct{}{}
		}
	}

	removed := 0
	var firstErr error
	for _, meta := range ix.cfg.Store.AllFiles() {
		if _, exists := onDisk[meta.Path]; !exists {
			if err := ix.cfg.Store.DeleteFile(meta.Path); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("prune %s: %w", meta.Path, err)
				}
				continue
			}
			removed++
			ix.log("removed", meta.Path, nil)
		}
	}
	return removed, firstErr
}

// relPath 把绝对路径转成相对项目根的 POSIX 风格路径。
func relPath(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	// 全流程统一用正斜杠,便于跨平台以及 LLM 输出的路径匹配
	return filepath.ToSlash(rel), nil
}
