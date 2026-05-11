package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"text/template"
)

// Skill 是一条已加载的 flywheel skill。它是纯数据 —— 没有 callback,没有
// 副作用。"执行 skill" 这件事完全由上层框架(harness)负责:框架拿到 Skill,
// 渲染 SOPTemplate,把结果作为 user prompt 喂给一个 sub-agent。
//
// 同名的新版本 Skill 通过 Registry.Add 覆盖旧版本。Go 的 reflect.StructOf
// 产出的类型是可被 GC 的(注:runtime 内部对每个唯一 struct shape 有一份
// 元数据级别的轻微缓存,但 KB 量级,与 plugin 模型相比可忽略),旧版本失去
// 引用后会被回收。
type Skill struct {
	// Name 是 skill 的唯一标识,等于 .go 文件中 type X struct {...} 的 X。
	// 这同时也是注册到 LLM 时的 toolcall name。
	Name string

	// Description 来自 type 声明的 doc 注释(// X is a skill that...)。
	// LLM 在选择 toolcall 时主要靠这个判断"这个工具是干嘛的"。
	Description string

	// StructType 是运行时构造出来的 reflect.Type。LLM 提交的 JSON 参数会
	// 反序列化进 reflect.New(StructType) 分配出来的实例里。
	StructType reflect.Type

	// SOPTemplate 是已经 parse 好的 text/template。模板的数据上下文是
	// reflect.New(StructType).Elem().Interface() —— 即填充好的 struct 值。
	// 模板里可以用 {{.FieldName}} 访问字段。
	SOPTemplate *template.Template

	// SOPSource 是模板的原文,只用于调试/日志/排错。
	SOPSource string

	// SourcePath 是 skill 来自哪个 .go 文件,用于错误回报。
	SourcePath string
}

// Registry 是一个线程安全的 skill 集合。dopharness 在初始化时(以及任何想
// 重新扫描 skill 目录的时机)调用 LoadDir,运行期通过 All() 取出注册到 Agent。
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill
}

func NewRegistry() *Registry {
	return &Registry{skills: map[string]*Skill{}}
}

// Add 注册或覆盖一条 skill。同名 skill 视为版本升级:新版本生效,旧版本
// 在 map 里被替换、失去引用后由 GC 回收。
func (r *Registry) Add(s *Skill) {
	if s == nil || s.Name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[s.Name] = s
}

// Get 按名字查找。
func (r *Registry) Get(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// All 返回当前所有 skill,按名字排序(便于稳定输出 / 测试断言)。
func (r *Registry) All() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove 显式移除一个 skill(典型场景:LLM 调用 remove_skill 工具时)。
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.skills[name]; !ok {
		return false
	}
	delete(r.skills, name)
	return true
}

// LoadDir 扫描一个目录下的所有 .go 文件,把每个文件里的 skill 都加载进来。
// 子目录递归扫描;_test.go 与隐藏文件(以 . 开头)忽略。
//
// 单个文件解析失败时返回错误,**已经成功加载的 skill 不会回滚** —— 这是
// 故意的:让 dopharness 可以一边修一边跑,坏 skill 不影响好 skill。
//
// 目录不存在时直接返回 nil(注册表空也是合法状态)。
func (r *Registry) LoadDir(dir string) error {
	if dir == "" {
		return nil
	}
	stat, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("%s 不是目录", dir)
	}

	return filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// 跳过 testdata / 隐藏目录
			base := filepath.Base(path)
			if path != dir && (base == "testdata" || strings.HasPrefix(base, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		skills, err := LoadSkillFile(path)
		if err != nil {
			return fmt.Errorf("加载 %s: %w", path, err)
		}
		for _, s := range skills {
			r.Add(s)
		}
		return nil
	})
}
