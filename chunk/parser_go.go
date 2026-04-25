package chunk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
)

// ParseGoFile 解析单个 Go 文件,返回不含 ID 的 Chunk 列表(ID 在 store.UpsertFile 时分配)。
//
// 切片粒度:仅顶层声明 —— 函数、方法、type(struct/interface/alias)。
// 嵌套函数、匿名函数、闭包不独立成 Chunk(v1 边界)。
//
// relPath 是相对项目根的路径,用于填 Chunk.FilePath;absPath 是磁盘真实路径,用于读文件。
func ParseGoFile(absPath, relPath string) ([]*Chunk, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("chunk: read %s: %w", absPath, err)
	}

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, absPath, content, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("chunk: parse %s: %w", absPath, err)
	}

	var out []*Chunk
	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = append(out, buildGoFuncChunk(d, fset, relPath, content))
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				// 常量、变量、import 暂不索引(v1 边界)
				continue
			}
			// 一个 "type (...)" 块里可能有多个 TypeSpec,每个独立成 chunk
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				out = append(out, buildGoTypeChunk(d, ts, fset, relPath, content))
			}
		}
	}
	return out, nil
}

// buildGoFuncChunk 构造函数/方法的 Chunk。
func buildGoFuncChunk(fn *ast.FuncDecl, fset *token.FileSet, relPath string, content []byte) *Chunk {
	name := fn.Name.Name
	kind := KindFunction

	// 方法:把 receiver 类型拼到 Name 前
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = KindMethod
		if recvType := receiverTypeName(fn.Recv.List[0].Type); recvType != "" {
			name = recvType + "." + name
		}
	}

	// 取声明起点时,优先使用 Doc 注释的起点,这样摘要和完整体都会带上文档注释
	startPos := fn.Pos()
	if fn.Doc != nil {
		startPos = fn.Doc.Pos()
	}
	start := fset.Position(startPos).Offset
	end := fset.Position(fn.End()).Offset
	body := string(content[start:end])

	skeleton := goFuncSkeleton(fn, fset, content, start)

	return &Chunk{
		FilePath:    relPath,
		Kind:        kind,
		Name:        name,
		Skeleton:    skeleton,
		Body:        body,
		Defines:     []string{name},
		Refs:        extractGoRefs(fn.Body),
		ContentHash: HashBody(body),
	}
}

// goFuncSkeleton 生成函数骨架:保留签名原文(含注释、泛型、多行参数列表),
// 函数体替换为 "{ /* ... */ }"。接口方法等无 Body 的保留全文。
func goFuncSkeleton(fn *ast.FuncDecl, fset *token.FileSet, content []byte, declStart int) string {
	if fn.Body == nil {
		// 接口方法、外部链接函数(asm)
		end := fset.Position(fn.End()).Offset
		return string(content[declStart:end])
	}
	sigEnd := fset.Position(fn.Body.Lbrace).Offset
	return string(content[declStart:sigEnd]) + "{ /* ... */ }"
}

// buildGoTypeChunk 构造 type 声明的 Chunk。
// 注意:type 没有"骨架/主体"之分,Skeleton 等于 Body。
func buildGoTypeChunk(decl *ast.GenDecl, ts *ast.TypeSpec, fset *token.FileSet, relPath string, content []byte) *Chunk {
	name := ts.Name.Name
	kind := KindType
	switch ts.Type.(type) {
	case *ast.StructType:
		kind = KindStruct
	case *ast.InterfaceType:
		kind = KindInterface
	}

	// 声明范围以 GenDecl 为准,这样 "type ( A struct {} B struct {} )" 的情况下
	// 我们会重复取整个块——这是 v1 可接受的简化,极少见且不影响 LLM 理解。
	//
	// 更严格的做法是取 TypeSpec 的 Pos/End,但那样会丢失 "type" 关键字和括号。
	// 折中:如果 GenDecl 只有一个 Spec,取整个 GenDecl;否则取 TypeSpec 并手动拼 "type " 前缀。
	var body string
	if len(decl.Specs) == 1 {
		startPos := decl.Pos()
		if decl.Doc != nil {
			startPos = decl.Doc.Pos()
		}
		start := fset.Position(startPos).Offset
		end := fset.Position(decl.End()).Offset
		body = string(content[start:end])
	} else {
		start := fset.Position(ts.Pos()).Offset
		end := fset.Position(ts.End()).Offset
		body = "type " + string(content[start:end])
	}

	return &Chunk{
		FilePath:    relPath,
		Kind:        kind,
		Name:        name,
		Skeleton:    body, // type 无需抽骨架
		Body:        body,
		Defines:     []string{name},
		Refs:        extractGoRefs(ts.Type),
		ContentHash: HashBody(body),
	}
}

// receiverTypeName 从 receiver 表达式中提取类型名,处理 *T 和 T 两种形式。
// 泛型 receiver 如 *List[T] 只提取 List。
func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		// List[T]
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		// Map[K, V]
		return receiverTypeName(t.X)
	}
	return ""
}

// extractGoRefs 遍历 AST 节点,收集被引用的标识符。
// 策略偏保守:只抓函数调用、选择器、复合字面量类型,避免把局部变量也算成 ref。
// 最终去重 + 过滤太短(< 2 字符)的名字。
func extractGoRefs(node ast.Node) []string {
	if node == nil {
		return nil
	}
	seen := map[string]struct{}{}

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch fun := x.Fun.(type) {
			case *ast.Ident:
				// foo()
				seen[fun.Name] = struct{}{}
			case *ast.SelectorExpr:
				// pkg.Foo() 或 obj.Method()
				seen[fun.Sel.Name] = struct{}{}
				if id, ok := fun.X.(*ast.Ident); ok {
					seen[id.Name] = struct{}{}
				}
			}
		case *ast.SelectorExpr:
			// x.Field
			seen[x.Sel.Name] = struct{}{}
		case *ast.CompositeLit:
			// T{...}
			switch t := x.Type.(type) {
			case *ast.Ident:
				seen[t.Name] = struct{}{}
			case *ast.SelectorExpr:
				seen[t.Sel.Name] = struct{}{}
			}
		}
		return true
	})

	out := make([]string, 0, len(seen))
	for k := range seen {
		if len(k) >= 2 {
			out = append(out, k)
		}
	}
	return out
}
