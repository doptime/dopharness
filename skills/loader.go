package skills

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf8"
)

// LoadSkillFile 把单个 .go 文件解析成 0 个或多个 *Skill。
//
// 一个 skill 由两部分组成:
//  1. 一个 type 声明: type X struct { ... }
//  2. 一个同名的常量声明: const XSOP = "..."  (字符串字面量;支持 raw 与
//     普通字符串)
//
// 两者的对应关系按名字匹配:type 名字加 "SOP" 后缀就是常量名。文件中可以有
// 多对 (type, SOP) 共存;type 没有匹配的 SOP 会被忽略(允许文件里出现纯
// 辅助类型)。
//
// type 的 doc 注释(紧贴在 type 声明上方的 // 注释)被当作 description,
// LLM 在选 toolcall 时主要靠它判断"这个工具是干嘛的"。
//
// 字段类型的限制见 builder.go 的 astTypeToReflect 注释。
func LoadSkillFile(path string) ([]*Skill, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Pass 1: 收集所有名为 <X>SOP 的字符串常量,key 是去掉 SOP 后缀的 type 名。
	sopByType := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasSuffix(name.Name, "SOP") || name.Name == "SOP" {
					continue
				}
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return nil, fmt.Errorf("文件 %s: 常量 %s 必须是字符串字面量(MVP 不支持表达式)",
						path, name.Name)
				}
				val, qerr := strconv.Unquote(lit.Value)
				if qerr != nil {
					return nil, fmt.Errorf("文件 %s: 常量 %s 字符串解析失败: %w",
						path, name.Name, qerr)
				}
				typeName := strings.TrimSuffix(name.Name, "SOP")
				sopByType[typeName] = val
			}
		}
	}

	// Pass 2: 收集所有 type 声明,与 SOP map 配对。
	var skills []*Skill
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				// 不是 struct(可能是 type X = ... 别名或其他),跳过
				continue
			}
			sopText, hasSop := sopByType[ts.Name.Name]
			if !hasSop {
				// 没有匹配的 SOP,这只是个普通辅助类型,不视为 skill
				continue
			}

			rt, err := BuildStructFromAST(st)
			if err != nil {
				return nil, fmt.Errorf("文件 %s, type %s: %w",
					path, ts.Name.Name, err)
			}

			tmpl, err := template.New(ts.Name.Name).Parse(sopText)
			if err != nil {
				return nil, fmt.Errorf("文件 %s, %s 的 SOP 模板解析失败: %w",
					path, ts.Name.Name, err)
			}

			// doc 注释:优先 ts.Doc(分组声明里),退到 gd.Doc(单独声明里)
			var doc string
			if ts.Doc != nil {
				doc = strings.TrimSpace(ts.Doc.Text())
			} else if gd.Doc != nil {
				doc = strings.TrimSpace(gd.Doc.Text())
			}
			// 按 Go 惯例 doc 是 "TypeName does X..." 形式,去掉前缀的类型名
			// 让 description 更直接
			doc = stripDocLeadingTypeName(doc, ts.Name.Name)

			skills = append(skills, &Skill{
				Name:        ts.Name.Name,
				Description: doc,
				StructType:  rt,
				SOPTemplate: tmpl,
				SOPSource:   sopText,
				SourcePath:  path,
			})
		}
	}

	return skills, nil
}

// stripDocLeadingTypeName 把 doc 注释开头的 "TypeName " 去掉。
//
//	"AddRoute 在 router 里新增一条路由"  ->  "在 router 里新增一条路由"
//
// 找不到匹配前缀就原样返回。
func stripDocLeadingTypeName(doc, typeName string) string {
	if doc == "" || typeName == "" {
		return doc
	}
	if !strings.HasPrefix(doc, typeName) {
		return doc
	}
	rest := doc[len(typeName):]
	if rest == "" {
		return doc // doc 就是类型名本身,保留
	}
	// 后面必须是空白字符,否则不是真前缀(防止误吃 AddRouteHandler 这种)
	r, _ := utf8.DecodeRuneInString(rest)
	if r == ' ' || r == '\t' || r == ':' || r == ',' || r == '。' || r == '.' {
		return strings.TrimSpace(strings.TrimLeft(rest, " \t:,。."))
	}
	return doc
}
