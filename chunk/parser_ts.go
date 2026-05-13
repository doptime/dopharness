package chunk

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/cespare/xxhash/v2"
)

//go:embed sidecar.ts
var embeddedSidecar []byte

// TSParser 是 TS/JS/TSX/JSX 文件的解析器。
//
// 工作方式:
//   - 首次使用时,把嵌入的 sidecar.ts 脚本写到系统临时目录(文件名含 hash,可重用)
//   - 每次调用启动一个 bun 进程,通过 stdin 喂文件路径,stdout 读 JSONL 结果
//
// 并发安全:解析方法本身可并发调用,每次会起新的 bun 进程。
// 但 sidecar 脚本的释放是惰性 + 一次性的,受内部 sync.Once 保护。
type TSParser struct {
	// runtime 是 bun 可执行路径。延迟到首次使用时探测。
	runtime     string
	runtimeOnce sync.Once
	runtimeErr  error

	// 脚本路径,释放一次后复用。
	scriptPath string
	scriptOnce sync.Once
	scriptErr  error

	// 允许通过环境变量覆盖运行时路径(测试 / 企业环境)
	// 优先级:DOPHARNESS_BUN > PATH 查找
	runtimeOverride string
}

// NewTSParser 构造一个新的 TS 解析器。
// 如果用户通过环境变量 DOPHARNESS_BUN 指定了 bun 路径,优先使用。
func NewTSParser() *TSParser {
	return &TSParser{
		runtimeOverride: os.Getenv("DOPHARNESS_BUN"),
	}
}

// ensureRuntime 确定 bun 可执行文件位置。幂等。
func (p *TSParser) ensureRuntime() error {
	p.runtimeOnce.Do(func() {
		if p.runtimeOverride != "" {
			// 用户显式指定,不做 PATH 查找,但校验可执行
			if _, err := os.Stat(p.runtimeOverride); err != nil {
				p.runtimeErr = fmt.Errorf("chunk: DOPHARNESS_BUN=%s not found: %w", p.runtimeOverride, err)
				return
			}
			p.runtime = p.runtimeOverride
			return
		}
		path, err := exec.LookPath("bun")
		if err != nil {
			p.runtimeErr = fmt.Errorf("chunk: bun not found in PATH — install from https://bun.sh or set DOPHARNESS_BUN")
			return
		}
		p.runtime = path
	})
	return p.runtimeErr
}

// ensureScript 把内嵌的 sidecar.ts 写到临时目录。
// 文件名中含内容 hash,内容变化自动用新文件,避免升级后跑到老脚本。
func (p *TSParser) ensureScript() error {
	p.scriptOnce.Do(func() {
		hash := xxhash.Sum64(embeddedSidecar)
		name := fmt.Sprintf("dopharness-sidecar-%016x.ts", hash)
		path := filepath.Join(os.TempDir(), name)

		// 如果已存在且内容匹配,直接用
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, embeddedSidecar) {
			p.scriptPath = path
			return
		}

		// 原子写入:先写 .tmp 再 rename
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, embeddedSidecar, 0o644); err != nil {
			p.scriptErr = fmt.Errorf("chunk: write sidecar: %w", err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			p.scriptErr = fmt.Errorf("chunk: finalize sidecar: %w", err)
			return
		}
		p.scriptPath = path
	})
	return p.scriptErr
}

// sidecarResult 对应 sidecar.ts 的 FileResult 输出。
//
// sidecar 可能仍会输出 skeleton 字段,我们的 unmarshal 忽略它(JSON decoder 对
// 未声明字段默认丢弃)。这样不需要同步改动 sidecar.ts —— 双方各自瘦身。
type sidecarResult struct {
	File   string `json:"file"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Chunks []struct {
		Kind string   `json:"kind"`
		Name string   `json:"name"`
		Body string   `json:"body"`
		Refs []string `json:"refs"`
	} `json:"chunks,omitempty"`
}

// ParseTSFile 解析单个 TS/JS 文件。便捷接口,内部走批量模式(传 1 个文件)。
//
// relPath 是写到 Chunk.FilePath 的相对路径;absPath 是 sidecar 读取的真实路径。
func (p *TSParser) ParseTSFile(absPath, relPath string) ([]*Chunk, error) {
	results, err := p.ParseTSFiles([]TSFileReq{{AbsPath: absPath, RelPath: relPath}})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("chunk: sidecar returned no result for %s", absPath)
	}
	r := results[0]
	if r.Err != nil {
		return nil, r.Err
	}
	return r.Chunks, nil
}

// TSFileReq 是批量解析的单项请求。
type TSFileReq struct {
	AbsPath string // 磁盘真实路径,sidecar 读它
	RelPath string // 相对项目根,写入 Chunk.FilePath
}

// TSFileResult 是批量解析的单项结果。
type TSFileResult struct {
	AbsPath string
	RelPath string
	Chunks  []*Chunk
	Err     error // 该文件的解析错误;其他文件不受影响
}

// ParseTSFiles 批量解析多个文件,单次 bun 启动。
//
// 这是主要的高性能入口。对百文件级别的项目,比逐文件调用快 3-5 倍
// (完全消除了 bun 冷启动开销)。
func (p *TSParser) ParseTSFiles(reqs []TSFileReq) ([]*TSFileResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if err := p.ensureRuntime(); err != nil {
		return nil, err
	}
	if err := p.ensureScript(); err != nil {
		return nil, err
	}

	// 建立 abs -> rel 的映射,sidecar 用 abs 路径,我们回填时用 rel
	relMap := make(map[string]string, len(reqs))
	var stdinBuf bytes.Buffer
	for _, r := range reqs {
		relMap[r.AbsPath] = r.RelPath
		stdinBuf.WriteString(r.AbsPath)
		stdinBuf.WriteByte('\n')
	}

	cmd := exec.Command(p.runtime, "run", p.scriptPath, "--batch")
	cmd.Stdin = &stdinBuf
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("chunk: bun sidecar failed: %w | stderr: %s", err, stderr.String())
	}

	// 按行解析 JSONL
	results := make([]*TSFileResult, 0, len(reqs))
	seen := make(map[string]bool, len(reqs))
	scanner := bufio.NewScanner(&stdout)
	// 默认 bufio 的 64KB 对大文件不够用,设到 8MB
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var raw sidecarResult
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf("chunk: parse sidecar output: %w | line: %s", err, string(line))
		}

		relPath := relMap[raw.File]
		seen[raw.File] = true

		res := &TSFileResult{
			AbsPath: raw.File,
			RelPath: relPath,
		}
		if !raw.OK {
			res.Err = fmt.Errorf("sidecar: %s", raw.Error)
			results = append(results, res)
			continue
		}

		chunks := make([]*Chunk, 0, len(raw.Chunks))
		for _, rc := range raw.Chunks {
			kind := Kind(rc.Kind)
			// 验证 kind 是已知值;未知值降级为 KindType 不报错
			switch kind {
			case KindFunction, KindMethod, KindClass, KindInterface, KindType, KindStruct:
				// ok
			default:
				kind = KindType
			}
			chunks = append(chunks, &Chunk{
				FilePath: relPath,
				Kind:     kind,
				Name:     rc.Name,
				Body:     rc.Body,
				Refs:     rc.Refs,
			})
		}
		res.Chunks = chunks
		results = append(results, res)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("chunk: read sidecar stdout: %w", err)
	}

	// 检查是否有请求没返回(sidecar 异常时可能少行)
	for _, r := range reqs {
		if !seen[r.AbsPath] {
			results = append(results, &TSFileResult{
				AbsPath: r.AbsPath,
				RelPath: r.RelPath,
				Err:     fmt.Errorf("sidecar did not return result for %s", r.AbsPath),
			})
		}
	}

	return results, nil
}

// TSRawError 是一条最小化的 TS 语法错误。
// 字段与 sidecar.ts --validate 的输出 JSON 对齐。
// 对外暴露是为了让 edit 包的 validator 能直接拿到(避免跨包循环依赖)。
type TSRawError struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// ValidateCode 调用 sidecar 的 --validate 模式,对一段代码做纯语法校验。
//
// 返回值:
//   - ok=true:语法 OK
//   - ok=false:errs 是错误列表(已按行号定位)
//   - err 非 nil:sidecar 进程本身异常(与代码无关)
func (p *TSParser) ValidateCode(code string, kind string) (ok bool, errs []TSRawError, err error) {
	if err := p.ensureRuntime(); err != nil {
		return false, nil, err
	}
	if err := p.ensureScript(); err != nil {
		return false, nil, err
	}
	if kind == "" {
		kind = "ts"
	}

	reqBytes, _ := json.Marshal(map[string]string{"code": code, "kind": kind})

	cmd := exec.Command(p.runtime, "run", p.scriptPath, "--validate")
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return false, nil, fmt.Errorf("chunk: validate sidecar: %w | stderr: %s", runErr, stderr.String())
	}

	var resp struct {
		OK     bool         `json:"ok"`
		Errors []TSRawError `json:"errors"`
	}
	// validate 只输出一行
	line := bytes.TrimSpace(stdout.Bytes())
	if len(line) == 0 {
		return false, nil, fmt.Errorf("chunk: validate sidecar produced no output")
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return false, nil, fmt.Errorf("chunk: parse validate response: %w | output: %s", err, stdout.String())
	}
	return resp.OK, resp.Errors, nil
}
