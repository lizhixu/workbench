/**
 * 轻量 Markdown 渲染（零依赖）。
 *
 * 用途：渲染来自 GitHub Release 的 release_notes，以及 AI 对话内容。
 * 支持范围（够覆盖本项目 release notes 与常规 GitHub notes）：
 *   - 标题 # / ## / ###…（映射为 h4~h6，避免在卡片里过大）
 *   - 无序列表 - * +、有序列表 1. / 1)
 *   - 列表项缩进续行（本项目 changelog 的换行排版依赖它）
 *   - 代码块 ```、行内代码 `code`
 *   - **加粗**、~~删除线~~、[文本](链接)
 *   - 引用 >、分隔线 ---
 *
 * 安全：先整体转义 HTML，再基于转义后的文本做标记替换，输出的标签全部由本模块
 * 生成；链接只放行 http(s)/mailto/锚点/站内绝对路径，其余（含 javascript:）一律降级为 #。
 * 不支持表格与行内 HTML —— release notes 用不到，且引入完整的 markdown 库会显著增加体积。
 */

export interface MarkdownRenderOptions {
  /** 代码块 <pre> 的类名，默认 md-code-block */
  codeBlockClass?: string
  /** 行内代码 <code> 的类名，默认 md-inline-code */
  inlineCodeClass?: string
}

const HTML_ESCAPES: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
}

function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (ch) => HTML_ESCAPES[ch] ?? ch)
}

function safeUrl(raw: string): string {
  const url = raw.trim().replace(/"/g, '%22').replace(/\s/g, '%20')
  if (/^(https?:\/\/|mailto:|#|\/)/i.test(url)) return url
  return '#'
}

function renderInline(text: string, inlineCodeClass: string): string {
  const codes: string[] = []
  // 行内代码先抽出来占位，避免其中的 ** 或链接语法被二次解析
  let out = escapeHtml(text).replace(/`([^`]+)`/g, (_match, code: string) => {
    codes.push(code)
    return `\u0000${codes.length - 1}\u0000`
  })

  out = out
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/__([^_]+)__/g, '<strong>$1</strong>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>')
    .replace(
      /\[([^\]]+)\]\(([^)\s]+)\)/g,
      (_match, label: string, url: string) =>
        `<a href="${safeUrl(url)}" target="_blank" rel="noopener noreferrer">${label}</a>`,
    )

  return out.replace(
    /\u0000(\d+)\u0000/g,
    (_match, index: string) => `<code class="${inlineCodeClass}">${codes[Number(index)]}</code>`,
  )
}

export function renderMarkdown(source: string, options: MarkdownRenderOptions = {}): string {
  if (!source) return ''

  const codeBlockClass = options.codeBlockClass ?? 'md-code-block'
  const inlineCodeClass = options.inlineCodeClass ?? 'md-inline-code'

  const lines = source.replace(/\r\n?/g, '\n').split('\n')
  const blocks: string[] = []

  let paragraph: string[] = []
  let listType: 'ul' | 'ol' | null = null
  let listItems: string[] = []
  let codeLines: string[] | null = null

  const flushParagraph = () => {
    if (!paragraph.length) return
    blocks.push(`<p>${paragraph.map((line) => renderInline(line, inlineCodeClass)).join('<br>')}</p>`)
    paragraph = []
  }

  const flushList = () => {
    if (!listType || !listItems.length) {
      listType = null
      listItems = []
      return
    }
    const items = listItems.map((item) => `<li>${renderInline(item, inlineCodeClass)}</li>`).join('')
    blocks.push(`<${listType}>${items}</${listType}>`)
    listType = null
    listItems = []
  }

  const flushCode = () => {
    if (codeLines === null) return
    blocks.push(`<pre class="${codeBlockClass}">${escapeHtml(codeLines.join('\n'))}</pre>`)
    codeLines = null
  }

  for (const rawLine of lines) {
    const line = rawLine.replace(/\s+$/, '')

    // 代码块围栏
    if (/^\s*```/.test(line)) {
      if (codeLines === null) {
        flushParagraph()
        flushList()
        codeLines = []
      } else {
        flushCode()
      }
      continue
    }
    if (codeLines !== null) {
      codeLines.push(rawLine)
      continue
    }

    // 空行：结束当前段落与列表
    if (!line.trim()) {
      flushParagraph()
      flushList()
      continue
    }

    // 分隔线
    if (/^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      flushParagraph()
      flushList()
      blocks.push('<hr>')
      continue
    }

    // 标题：# 映射为 h4，## → h5，### 及更深 → h6
    const heading = /^(#{1,6})\s+(.*)$/.exec(line)
    if (heading) {
      flushParagraph()
      flushList()
      const level = Math.min(6, heading[1].length + 3)
      blocks.push(`<h${level}>${renderInline(heading[2], inlineCodeClass)}</h${level}>`)
      continue
    }

    // 引用
    const quote = /^\s*>\s?(.*)$/.exec(line)
    if (quote) {
      flushParagraph()
      flushList()
      blocks.push(`<blockquote>${renderInline(quote[1], inlineCodeClass)}</blockquote>`)
      continue
    }

    // 无序列表
    const bullet = /^\s*[-*+]\s+(.*)$/.exec(line)
    if (bullet) {
      flushParagraph()
      if (listType !== 'ul') {
        flushList()
        listType = 'ul'
      }
      listItems.push(bullet[1])
      continue
    }

    // 有序列表
    const ordered = /^\s*\d+[.)]\s+(.*)$/.exec(line)
    if (ordered) {
      flushParagraph()
      if (listType !== 'ol') {
        flushList()
        listType = 'ol'
      }
      listItems.push(ordered[1])
      continue
    }

    // 列表项续行：缩进 2 空格以上且当前处于列表中
    if (listType && /^\s{2,}\S/.test(rawLine)) {
      const previous = listItems[listItems.length - 1]
      const continuation = line.trim()
      // 中文换行拼接不加空格，英文单词之间才补一个
      const needsSpace = /[A-Za-z0-9]$/.test(previous) && /^[A-Za-z0-9]/.test(continuation)
      listItems[listItems.length - 1] = needsSpace ? `${previous} ${continuation}` : `${previous}${continuation}`
      continue
    }

    // 普通段落
    flushList()
    paragraph.push(line.trim())
  }

  flushParagraph()
  flushList()
  flushCode()

  return blocks.join('')
}
