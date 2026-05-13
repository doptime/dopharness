
// sidecar.ts —— dopharness 的 TypeScript/JavaScript AST 分析器。
//
// 运行方式(由 Go 端调用):
//   bun run sidecar.ts --batch     # 从 stdin 逐行读文件路径,每行输出一份 JSON(JSONL)
//   bun run sidecar.ts --single <path>  # 分析单个文件,输出单份 JSON
//
// 输出 schema(单份):
//   { "file": "<path>", "ok": true,  "chunks": [ { id, kind, name, skeleton, body, refs }, ... ] }
//   { "file": "<path>", "ok": false, "error": "<msg>" }
//
// 注意:
//   - ID 字段留空字符串,由 Go 端的 store.UpsertFile 统一分配,保证 ID 空间全局唯一
//   - refs 用轻量启发式收集(标识符词法扫描 + 关键字过滤),和 Go 端的策略对齐
//   - 切片粒度:顶层函数/类/接口/类型别名 + 类内方法。匿名函数、嵌套函数不独立切
//
// 为什么不依赖项目的 tsconfig:我们只做单文件语法级分析,不做类型检查。
// 这样分析器对任何项目都能跑,不会因为缺少依赖或配置错误就失败。

import ts from "typescript";
import { readFileSync } from "fs";

type ChunkOut = {
  id: string;
  kind: "Function" | "Method" | "Class" | "Interface" | "Type";
  name: string;
  skeleton: string;
  body: string;
  refs: string[];
};

type FileResult =
  | { file: string; ok: true; chunks: ChunkOut[] }
  | { file: string; ok: false; error: string };

// JavaScript/TypeScript 的常见关键字与内置标识符,从 refs 中过滤掉以降噪
const KEYWORDS = new Set([
  "function", "const", "let", "var", "return", "if", "else", "for", "while",
  "do", "switch", "case", "break", "continue", "class", "interface", "type",
  "extends", "implements", "import", "export", "from", "as", "default",
  "null", "undefined", "true", "false", "new", "this", "super", "void",
  "public", "private", "protected", "readonly", "static", "abstract",
  "try", "catch", "finally", "throw", "async", "await", "yield",
  "typeof", "instanceof", "in", "of", "delete",
  "string", "number", "boolean", "any", "unknown", "never", "object",
  // 常见全局,去掉减少噪音
  "console", "log", "error", "warn",
]);

/**
 * 从源码片段中用正则提取标识符,做集合去重并过滤关键字/过短词。
 * 这是一个比较"脏"的启发式,但对 LLM 定位符号已足够。
 */
function extractRefs(source: string): string[] {
  const tokens = source.match(/[A-Za-z_$][\w$]*/g) || [];
  const set = new Set<string>();
  for (const t of tokens) {
    if (t.length < 2) continue;
    if (KEYWORDS.has(t)) continue;
    set.add(t);
  }
  return Array.from(set);
}

/**
 * 生成函数/方法的骨架:签名原文 + "{ /* ... *\/ }"。
 * 通过 node.body.getStart() 精确拿到签名末尾 offset,避免用字符串 indexOf('{') 在泛型里误切。
 */
function funcSkeleton(
  sourceCode: string,
  node: ts.FunctionLikeDeclaration,
  fullStart: number,
  fullEnd: number
): string {
  // 无函数体(接口方法、abstract 方法、overload 签名):整段就是骨架
  if (!node.body) {
    return sourceCode.slice(fullStart, fullEnd);
  }
  const bodyStart = node.body.getStart();
  const signaturePart = sourceCode.slice(fullStart, bodyStart);
  return signaturePart + "{ /* ... */ }";
}

/**
 * 取一个节点的 "完整起点" —— 包括紧邻其上的 JSDoc / 前导注释。
 * ts 的 node.getStart() 默认跳过前导 trivia;我们要 node.getFullStart() 然后
 * 再微调,避免把上一个声明之间的空行也吃进来。
 *
 * 简单策略:用 getFullStart() 然后 skip 掉开头的空行,保留 // 和 /* 注释。
 */
function declStart(sourceCode: string, node: ts.Node): number {
  const fullStart = node.getFullStart();
  const realStart = node.getStart();

  // 在 [fullStart, realStart) 里找最后一个纯空白行的结尾,作为本次声明的起点
  let cut = fullStart;
  let i = fullStart;
  while (i < realStart) {
    // 跳到行尾
    const nl = sourceCode.indexOf("\n", i);
    const lineEnd = nl === -1 || nl >= realStart ? realStart : nl + 1;
    const line = sourceCode.slice(i, lineEnd);
    // 如果这一行是纯空白(可能含 \r\n),把 cut 推到行尾 —— 意味着我们跳过了它
    if (/^\s*$/.test(line)) {
      cut = lineEnd;
    } else {
      // 第一次出现非空白行,就是我们的起点(通常是 /** 或 // 开头)
      break;
    }
    i = lineEnd;
  }
  return cut;
}

/**
 * 分析单个文件,返回 chunk 列表。不抛异常,错误包装到返回值里。
 */
function analyzeFile(filePath: string): FileResult {
  let sourceCode: string;
  try {
    sourceCode = readFileSync(filePath, "utf-8");
  } catch (e: any) {
    return { file: filePath, ok: false, error: `read failed: ${e.message}` };
  }

  let sourceFile: ts.SourceFile;
  try {
    sourceFile = ts.createSourceFile(
      filePath,
      sourceCode,
      ts.ScriptTarget.Latest,
      /*setParentNodes*/ true,
      filePath.endsWith(".tsx") || filePath.endsWith(".jsx")
        ? ts.ScriptKind.TSX
        : undefined
    );
  } catch (e: any) {
    return { file: filePath, ok: false, error: `parse failed: ${e.message}` };
  }

  const chunks: ChunkOut[] = [];

  /**
   * 处理一个声明节点,产出 0 或 1 个 ChunkOut。
   * container 参数用于给 class 内的方法打 "ClassName.methodName" 这样的限定名。
   */
  function emit(
    node: ts.Node,
    kind: ChunkOut["kind"],
    name: string,
    container?: string
  ): void {
    const start = declStart(sourceCode, node);
    const end = node.getEnd();
    const body = sourceCode.slice(start, end);

    let skeleton = body;
    if (
      ts.isFunctionDeclaration(node) ||
      ts.isMethodDeclaration(node) ||
      ts.isConstructorDeclaration(node) ||
      ts.isGetAccessorDeclaration(node) ||
      ts.isSetAccessorDeclaration(node)
    ) {
      skeleton = funcSkeleton(sourceCode, node as ts.FunctionLikeDeclaration, start, end);
    } else if (ts.isClassDeclaration(node)) {
      // Class 的骨架:保留声明头 + { ... }
      // 取 class 名、extends、implements 的部分,然后用 { ... } 代替整个 body
      const classNode = node as ts.ClassDeclaration;
      const openBrace = classNode
        .getChildren()
        .find((c) => c.kind === ts.SyntaxKind.OpenBraceToken);
      if (openBrace) {
        const headerEnd = openBrace.getStart();
        skeleton = sourceCode.slice(start, headerEnd) + "{ /* ... */ }";
      }
    }

    const qualifiedName = container ? `${container}.${name}` : name;

    chunks.push({
      id: "", // 由 Go 端分配
      kind,
      name: qualifiedName,
      skeleton,
      body,
      refs: extractRefs(body),
    });
  }

  /**
   * 遍历 source file 的顶层语句。
   * 进入 class declaration 时下钻一层,产出其方法;不再深入。
   */
  function visitTopLevel(node: ts.Node): void {
    if (ts.isFunctionDeclaration(node) && node.name) {
      emit(node, "Function", node.name.text);
      return;
    }
    if (ts.isClassDeclaration(node) && node.name) {
      const className = node.name.text;
      emit(node, "Class", className);
      // 进入 class 体,产出方法
      for (const member of node.members) {
        if (ts.isMethodDeclaration(member) && member.name) {
          emit(member, "Method", memberName(member.name), className);
        } else if (ts.isConstructorDeclaration(member)) {
          emit(member, "Method", "constructor", className);
        } else if (
          ts.isGetAccessorDeclaration(member) ||
          ts.isSetAccessorDeclaration(member)
        ) {
          if (member.name) {
            const prefix = ts.isGetAccessorDeclaration(member) ? "get " : "set ";
            emit(member, "Method", prefix + memberName(member.name), className);
          }
        }
      }
      return;
    }
    if (ts.isInterfaceDeclaration(node)) {
      emit(node, "Interface", node.name.text);
      return;
    }
    if (ts.isTypeAliasDeclaration(node)) {
      emit(node, "Type", node.name.text);
      return;
    }
    // 支持 `export const foo = () => { ... }` 这种常见模式
    if (ts.isVariableStatement(node)) {
      for (const decl of node.declarationList.declarations) {
        if (
          ts.isIdentifier(decl.name) &&
          decl.initializer &&
          (ts.isArrowFunction(decl.initializer) ||
            ts.isFunctionExpression(decl.initializer))
        ) {
          // 把整个 VariableStatement 作为 body,这样能保留 export/const
          const name = decl.name.text;
          const start = declStart(sourceCode, node);
          const end = node.getEnd();
          const body = sourceCode.slice(start, end);

          // 骨架:定位 initializer 的 body
          const fn = decl.initializer as ts.FunctionLikeDeclaration;
          let skeleton = body;
          if (fn.body && ts.isBlock(fn.body)) {
            const bodyStart = fn.body.getStart();
            skeleton = sourceCode.slice(start, bodyStart) + "{ /* ... */ }";
          }

          chunks.push({
            id: "",
            kind: "Function",
            name,
            skeleton,
            body,
            refs: extractRefs(body),
          });
        }
      }
      return;
    }
    // 其余顶层语句(import/export 再导出等)暂不索引
  }

  function memberName(n: ts.PropertyName): string {
    if (ts.isIdentifier(n)) return n.text;
    if (ts.isStringLiteral(n)) return n.text;
    if (ts.isNumericLiteral(n)) return n.text;
    // computed name 罕见,简化处理
    return n.getText();
  }

  sourceFile.forEachChild(visitTopLevel);

  return { file: filePath, ok: true, chunks };
}

/**
 * validateCode 对一段 TS/JS 源码做语法级校验。
 *
 * 实现方式:用 ts.createSourceFile + TS 编译器的 parseDiagnostics。
 * 这样不需要 tsconfig,也不做类型检查 —— 只看语法能否 parse。
 *
 * 类型错误(如 unknown identifier)不会报出,这符合 dopharness 的定位:
 * 我们只保证 LLM 产出的代码"能被 parser 接受",进一步的类型检查交给用户的 CI。
 */
function validateCode(
  code: string,
  kind: string
): { ok: boolean; errors: { line: number; column: number; message: string }[] } {
  const scriptKind = kindToScriptKind(kind);
  // 随便起个文件名,不会写盘
  const fakeName = `__validate__.${kind}`;
  const sf = ts.createSourceFile(
    fakeName,
    code,
    ts.ScriptTarget.Latest,
    /*setParentNodes*/ false,
    scriptKind
  );
  // ts.createSourceFile 在 parse 失败时并不抛异常,而是把 diagnostics 放在 sf 上
  // @ts-ignore —— parseDiagnostics 是内部字段,在多数 TS 版本中都存在
  const diags: ts.Diagnostic[] = (sf as any).parseDiagnostics || [];
  if (diags.length === 0) {
    return { ok: true, errors: [] };
  }
  const errors = diags.map((d) => {
    let line = 0;
    let column = 0;
    if (d.file && typeof d.start === "number") {
      const lc = d.file.getLineAndCharacterOfPosition(d.start);
      line = lc.line + 1;
      column = lc.character + 1;
    }
    const msg = typeof d.messageText === "string"
      ? d.messageText
      : d.messageText.messageText;
    return { line, column, message: msg };
  });
  return { ok: false, errors };
}

function kindToScriptKind(kind: string): ts.ScriptKind {
  switch (kind.toLowerCase()) {
    case "tsx":
      return ts.ScriptKind.TSX;
    case "js":
      return ts.ScriptKind.JS;
    case "jsx":
      return ts.ScriptKind.JSX;
    default:
      return ts.ScriptKind.TS;
  }
}

// ---- CLI 入口 ----

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const mode = args[0];

  if (mode === "--single") {
    const path = args[1];
    if (!path) {
      process.stderr.write("usage: sidecar.ts --single <path>\n");
      process.exit(2);
    }
    const res = analyzeFile(path);
    process.stdout.write(JSON.stringify(res) + "\n");
    return;
  }

  if (mode === "--batch") {
    // 从 stdin 按行读文件路径,每条输出一行 JSON(JSONL 协议)
    // 用 Bun 的 Bun.stdin 或 Node 兼容的 process.stdin
    const input = await readAllStdin();
    const paths = input.split("\n").map((s) => s.trim()).filter((s) => s.length > 0);
    for (const p of paths) {
      const res = analyzeFile(p);
      process.stdout.write(JSON.stringify(res) + "\n");
    }
    return;
  }

  if (mode === "--validate") {
    // stdin 读 JSON: {"code": "...", "kind": "ts"|"tsx"|"js"|"jsx"}
    // stdout 输出 JSON: {"ok": bool, "errors": [{"line":int, "column":int, "message":str}]}
    const input = await readAllStdin();
    let req: { code: string; kind?: string };
    try {
      req = JSON.parse(input);
    } catch (e: any) {
      process.stdout.write(
        JSON.stringify({ ok: false, errors: [{ line: 0, column: 0, message: `bad validate request: ${e.message}` }] }) + "\n"
      );
      return;
    }
    const result = validateCode(req.code || "", req.kind || "ts");
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }

  process.stderr.write(
    `unknown mode: ${mode || "<empty>"}\nusage: sidecar.ts --single <path> | --batch | --validate\n`
  );
  process.exit(2);
}

async function readAllStdin(): Promise<string> {
  // Bun 有 Bun.stdin,但为兼容性,用 Node 风格的 stream 读取
  return new Promise((resolve, reject) => {
    let buf = "";
    process.stdin.setEncoding("utf-8");
    process.stdin.on("data", (chunk) => (buf += chunk));
    process.stdin.on("end", () => resolve(buf));
    process.stdin.on("error", reject);
  });
}

main().catch((e) => {
  process.stderr.write(`fatal: ${e?.message || e}\n`);
  process.exit(1);
});
