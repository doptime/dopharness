package memory

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ============================================================
// L0 - Meta Rules
// ============================================================

// MetaRulesLayer 读取单个 Markdown 文件作为核心规则。
// 典型内容:"禁止删除没被明确提到的代码"、"修改必须通过 Tool 调用"等。
type MetaRulesLayer struct {
	// Path 是规则文件路径。文件不存在时返回空字符串(不报错),便于首次使用。
	Path string

	// Fallback 是文件不存在时的默认规则文本。
	// 如果也为空,该层什么都不输出。
	Fallback string

	mu    sync.RWMutex
	cache string // 简单文件级缓存,按 mtime 失效
	mtime int64
}

func (l *MetaRulesLayer) Prefix() string { return "L0 Meta Rules" }

func (l *MetaRulesLayer) Render(ctx *RenderCtx) (string, string, error) {
	text, err := l.load()
	if err != nil {
		return "", "", err
	}
	if text == "" {
		return "", "", nil
	}
	// 包一层 XML 标签,让 LLM 识别边界
	wrapped := "<meta_rules>\n" + strings.TrimRight(text, "\n") + "\n</meta_rules>"
	return wrapped, "", nil
}

// load 读取并缓存规则文本。mtime 变化自动重载。
func (l *MetaRulesLayer) load() (string, error) {
	if l.Path == "" {
		return l.Fallback, nil
	}

	stat, err := os.Stat(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return l.Fallback, nil
		}
		return "", fmt.Errorf("memory: stat %s: %w", l.Path, err)
	}
	mt := stat.ModTime().Unix()

	l.mu.RLock()
	if l.mtime == mt && l.cache != "" {
		out := l.cache
		l.mu.RUnlock()
		return out, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	// 双重检查
	if l.mtime == mt && l.cache != "" {
		return l.cache, nil
	}
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return "", fmt.Errorf("memory: read %s: %w", l.Path, err)
	}
	l.cache = string(data)
	l.mtime = mt
	return l.cache, nil
}

// ============================================================
// L1 - Insight Index (from gateway)
// ============================================================

// InsightIndexLayer 把 gateway 产出的三态字符串作为 L1 输出。
// 它本身不做决策,只是把 ctx.ContextFromGateway 包上合适的标签,放到 user prompt 里。
type InsightIndexLayer struct{}

func (l *InsightIndexLayer) Prefix() string { return "L1 Insight Index" }

func (l *InsightIndexLayer) Render(ctx *RenderCtx) (string, string, error) {
	if ctx.ContextFromGateway == "" {
		return "", "", nil
	}
	// gateway 已经自己加了 <ignored_chunks>/<skeleton_chunks>/<full_chunks> 标签,
	// 我们只在外层再套一个 <project_context> 标明它是 L1 层。
	wrapped := "<project_context>\n" +
		strings.TrimRight(ctx.ContextFromGateway, "\n") +
		"\n</project_context>"
	return "", wrapped, nil
}

// ============================================================
// L2 - Global Facts
// ============================================================

// GlobalFactsLayer 读取一个 Markdown 文件,作为项目级稳定事实。
// 例:"本项目使用 Go 1.22、Redis 7、依赖注入模式由 wire 生成"。
type GlobalFactsLayer struct {
	Path     string
	Fallback string

	mu    sync.RWMutex
	cache string
	mtime int64
}

func (l *GlobalFactsLayer) Prefix() string { return "L2 Global Facts" }

func (l *GlobalFactsLayer) Render(ctx *RenderCtx) (string, string, error) {
	text, err := l.load()
	if err != nil {
		return "", "", err
	}
	if text == "" {
		return "", "", nil
	}
	wrapped := "<global_facts>\n" + strings.TrimRight(text, "\n") + "\n</global_facts>"
	// 放 system:事实通常很稳定,且影响所有决策
	return wrapped, "", nil
}

func (l *GlobalFactsLayer) load() (string, error) {
	if l.Path == "" {
		return l.Fallback, nil
	}
	stat, err := os.Stat(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return l.Fallback, nil
		}
		return "", fmt.Errorf("memory: stat %s: %w", l.Path, err)
	}
	mt := stat.ModTime().Unix()
	l.mu.RLock()
	if l.mtime == mt && l.cache != "" {
		out := l.cache
		l.mu.RUnlock()
		return out, nil
	}
	l.mu.RUnlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mtime == mt && l.cache != "" {
		return l.cache, nil
	}
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return "", fmt.Errorf("memory: read %s: %w", l.Path, err)
	}
	l.cache = string(data)
	l.mtime = mt
	return l.cache, nil
}

// ============================================================
// L3 - Task Skills
// ============================================================

// Skill 是一条可复用的 SOP 记录。
type Skill struct {
	Name    string // 从文件名推出(不含 .md 后缀)
	Path    string // 磁盘路径
	Content string // 文件内容
}

// SkillSelector 是 L3 的扩展点。
// 输入:用户 prompt + 所有已加载 skill;输出:本次要放进 prompt 的 skill 子集。
//
// v1 默认实现是 AllSkills(全量注入)。高级用户可以替换成:
//   - 关键字匹配
//   - LLM 预选
//   - 向量相似度
type SkillSelector func(userPrompt string, all []*Skill) []*Skill

// AllSkills 是默认选择器:全量放入。
func AllSkills(_ string, all []*Skill) []*Skill { return all }

// TaskSkillsLayer 从一个目录读取所有 .md 文件作为 skill 集合。
type TaskSkillsLayer struct {
	// Dir 是 skill 目录。不存在时视作空集合。
	Dir string

	// Selector 决定本次要用哪些 skill。nil 时默认 AllSkills。
	Selector SkillSelector

	mu        sync.RWMutex
	cached    []*Skill
	dirMtime  int64
	loadedOk  bool // 防止重复爆 error 日志
}

func (l *TaskSkillsLayer) Prefix() string { return "L3 Task Skills" }

func (l *TaskSkillsLayer) Render(ctx *RenderCtx) (string, string, error) {
	skills, err := l.loadAll()
	if err != nil {
		return "", "", err
	}
	if len(skills) == 0 {
		return "", "", nil
	}

	selector := l.Selector
	if selector == nil {
		selector = AllSkills
	}
	selected := selector(ctx.UserPrompt, skills)
	if len(selected) == 0 {
		return "", "", nil
	}

	var sb strings.Builder
	sb.WriteString("<task_skills count=\"")
	fmt.Fprintf(&sb, "%d", len(selected))
	sb.WriteString("\">\n")
	for _, sk := range selected {
		fmt.Fprintf(&sb, "<skill name=%q>\n", sk.Name)
		sb.WriteString(strings.TrimRight(sk.Content, "\n"))
		sb.WriteString("\n</skill>\n")
	}
	sb.WriteString("</task_skills>")
	return "", sb.String(), nil
}

// loadAll 扫描目录,返回所有 skill。简单缓存:目录 mtime 未变就复用。
func (l *TaskSkillsLayer) loadAll() ([]*Skill, error) {
	if l.Dir == "" {
		return nil, nil
	}
	stat, err := os.Stat(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: stat %s: %w", l.Dir, err)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("memory: %s is not a directory", l.Dir)
	}
	dirMt := stat.ModTime().Unix()

	l.mu.RLock()
	if l.loadedOk && l.dirMtime == dirMt {
		out := l.cached
		l.mu.RUnlock()
		return out, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadedOk && l.dirMtime == dirMt {
		return l.cached, nil
	}

	var skills []*Skill
	err = filepath.WalkDir(l.Dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		skills = append(skills, &Skill{
			Name:    name,
			Path:    path,
			Content: string(data),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("memory: walk skills: %w", err)
	}
	// 稳定顺序(按 Name)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	l.cached = skills
	l.dirMtime = dirMt
	l.loadedOk = true
	return skills, nil
}

// ============================================================
// L4 - Session Records
// ============================================================

// SessionRecord 是一次历史会话的压缩摘要。
// 存储为 JSON,方便未来扩展字段(如 tags、outcome、duration)。
type SessionRecord struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"ts"`    // Unix 秒
	Summary   string `json:"summary"`
}

// SessionRecordsLayer 从一个目录读取所有 .json 文件作为历史会话。
// 按时间戳倒序选取最近 Limit 条。
type SessionRecordsLayer struct {
	Dir   string
	Limit int // 0 或负 = 5 条

	mu       sync.RWMutex
	cached   []*SessionRecord
	dirMtime int64
	loadedOk bool
}

func (l *SessionRecordsLayer) Prefix() string { return "L4 Session Records" }

func (l *SessionRecordsLayer) Render(ctx *RenderCtx) (string, string, error) {
	records, err := l.loadAll()
	if err != nil {
		return "", "", err
	}
	if len(records) == 0 {
		return "", "", nil
	}
	limit := l.Limit
	if limit <= 0 {
		limit = 5
	}
	// 按时间戳倒序
	sort.Slice(records, func(i, j int) bool { return records[i].Timestamp > records[j].Timestamp })
	if len(records) > limit {
		records = records[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<session_history count=\"%d\">\n", len(records))
	for _, r := range records {
		fmt.Fprintf(&sb, "<session id=%q ts=\"%d\">\n", r.ID, r.Timestamp)
		sb.WriteString(strings.TrimRight(r.Summary, "\n"))
		sb.WriteString("\n</session>\n")
	}
	sb.WriteString("</session_history>")
	return "", sb.String(), nil
}

// Append 追加一条会话记录到磁盘(文件名 = ID.json)。
// 这是历史的写入入口,Memory 的调用方(harness)在每次请求完成后可以调用它。
func (l *SessionRecordsLayer) Append(rec *SessionRecord) error {
	if l.Dir == "" {
		return fmt.Errorf("memory: L4 Dir is empty")
	}
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("memory: invalid session record")
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", l.Dir, err)
	}
	path := filepath.Join(l.Dir, rec.ID+".json")
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 失效缓存
	l.mu.Lock()
	l.loadedOk = false
	l.mu.Unlock()
	return nil
}

func (l *SessionRecordsLayer) loadAll() ([]*SessionRecord, error) {
	if l.Dir == "" {
		return nil, nil
	}
	stat, err := os.Stat(l.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: stat %s: %w", l.Dir, err)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("memory: %s is not a directory", l.Dir)
	}
	dirMt := stat.ModTime().Unix()

	l.mu.RLock()
	if l.loadedOk && l.dirMtime == dirMt {
		out := l.cached
		l.mu.RUnlock()
		return out, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadedOk && l.dirMtime == dirMt {
		return l.cached, nil
	}

	var records []*SessionRecord
	err = filepath.WalkDir(l.Dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// 单文件读失败不致命
			return nil
		}
		var rec SessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			// 单文件格式错不致命
			return nil
		}
		records = append(records, &rec)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("memory: walk sessions: %w", err)
	}

	l.cached = records
	l.dirMtime = dirMt
	l.loadedOk = true
	return records, nil
}

// ============================================================
// 便捷构造器:根据一个根目录批量拉起所有层
// ============================================================

// LayoutDirs 是标准记忆目录布局。
//   <root>/rules.md      -> L0
//   <root>/facts.md      -> L2
//   <root>/skills/       -> L3
//   <root>/sessions/     -> L4
type LayoutDirs struct {
	Root string
}

// DefaultRulesFallback 给 L0 的兜底文本。
// 如果用户没放 rules.md,至少给 LLM 几条核心戒律。
const DefaultRulesFallback = `你在使用 dopharness 系统。请严格遵守以下规则:

1. 所有代码修改必须通过 Tool Call 提交,不要在自然语言里写"我会修改 X 文件"这种话。
2. 修改前先确认 chunk ID 准确;ID 是 4 字符标识,不要写成路径。
3. 如果需要新增文件,使用 CREATE_FILE;往已有文件追加声明,使用 ADD_CHUNK;
   修改已有声明,使用 MODIFY;不要混用。
4. 不要修改 <project_context> 中未出现的代码 —— 如果你需要看更多代码,
   请在回答中明确说明,不要编造。
5. 输出的代码必须是完整的顶层声明(含签名和主体)。
`

// BuildStandardMemory 按 LayoutDirs 的约定一次性构造好 5 层。
// 调用方通常就用这个就够了。
func BuildStandardMemory(layout LayoutDirs) *Memory {
	root := layout.Root
	return &Memory{
		L0: &MetaRulesLayer{
			Path:     filepath.Join(root, "rules.md"),
			Fallback: DefaultRulesFallback,
		},
		L1: &InsightIndexLayer{},
		L2: &GlobalFactsLayer{
			Path: filepath.Join(root, "facts.md"),
		},
		L3: &TaskSkillsLayer{
			Dir: filepath.Join(root, "skills"),
		},
		L4: &SessionRecordsLayer{
			Dir:   filepath.Join(root, "sessions"),
			Limit: 5,
		},
	}
}
