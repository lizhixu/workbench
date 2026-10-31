/**
 * 轻量 Markdown 渲染（零依赖）。
 *
 * 用途：渲染来自 GitHub Release 的 release_notes，以及 AI 对话内容。
 * 支持范围：
 *   - 标题 # / ## / ###…（映射为 h4~h6，避免在卡片里过大）
 *   - 无序列表 - * +、有序列表 1. / 1)
 *   - 列表项缩进续行（本项目 changelog 的换行排版依赖它）
 *   - 代码块 ```（支持语言标记 data-lang）、行内代码 `code`
 *   - **加粗**、*斜体*、~~删除线~~、[文本](链接)
 *   - GFM 表格 | Col 1 | Col 2 |（自动响应式 .md-table-wrap 包裹）
 *   - 引用 >、分隔线 ---
 *
 * 安全：先整体转义 HTML，再基于转义后的文本做标记替换，输出的标签全部由本模块
 * 生成；链接只放行 http(s)/mailto/锚点/站内绝对路径，其余（含 javascript:）一律降级为 #。
 * 纯零依赖，不引入 marked / markdown-it 等庞大依赖。
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
    .replace(/(^|[^*])\*([^*]+)\*([^*]|$)/g, '$1<em>$2</em>$3')
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

function splitCells(row: string): string[] {
  let trimmed = row.trim()
  if (trimmed.startsWith('|')) trimmed = trimmed.slice(1)
  if (trimmed.endsWith('|')) trimmed = trimmed.slice(0, -1)
  return trimmed
    .replace(/\\\|/g, '\u0001')
    .split('|')
    .map((c) => c.replace(/\u0001/g, '|').trim())
}

function isDelimiterRow(row: string): boolean {
  const cells = splitCells(row)
  if (cells.length === 0) return false
  return cells.every((c) => /^:?-+:?$/.test(c))
}

function getAlignments(delimRow: string): (string | null)[] {
  return splitCells(delimRow).map((c) => {
    const left = c.startsWith(':')
    const right = c.endsWith(':')
    if (left && right) return 'center'
    if (right) return 'right'
    if (left) return 'left'
    return null
  })
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
  let currentCodeLang = ''
  let tableLines: string[] | null = null

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
    const langAttr = currentCodeLang ? ` data-lang="${escapeHtml(currentCodeLang)}"` : ''
    blocks.push(`<pre class="${codeBlockClass}"${langAttr}><code>${escapeHtml(codeLines.join('\n'))}</code></pre>`)
    codeLines = null
    currentCodeLang = ''
  }

  const flushTable = () => {
    if (!tableLines || tableLines.length < 2) {
      if (tableLines && tableLines.length === 1) {
        paragraph.push(tableLines[0])
        flushParagraph()
      }
      tableLines = null
      return
    }
    const headers = splitCells(tableLines[0])
    const alignments = getAlignments(tableLines[1])
    const rows = tableLines.slice(2).map(splitCells)

    const thead =
      '<thead><tr>' +
      headers
        .map((h, i) => {
          const align = alignments[i] ? ` align="${alignments[i]}"` : ''
          return `<th${align}>${renderInline(h, inlineCodeClass)}</th>`
        })
        .join('') +
      '</tr></thead>'

    const tbody =
      '<tbody>' +
      rows
        .map(
          (r) =>
            '<tr>' +
            r
              .map((c, i) => {
                const align = alignments[i] ? ` align="${alignments[i]}"` : ''
                return `<td${align}>${renderInline(c, inlineCodeClass)}</td>`
              })
              .join('') +
            '</tr>',
        )
        .join('') +
      '</tbody>'

    blocks.push(`<div class="md-table-wrap"><table class="md-table">${thead}${tbody}</table></div>`)
    tableLines = null
  }

  for (let idx = 0; idx < lines.length; idx++) {
    const rawLine = lines[idx]
    const line = rawLine.replace(/\s+$/, '')

    // 代码块围栏
    const fence = /^\s*```(\w*)/.exec(line)
    if (fence && codeLines === null) {
      flushParagraph()
      flushList()
      flushTable()
      currentCodeLang = fence[1] || ''
      codeLines = []
      continue
    } else if (/^\s*```/.test(line) && codeLines !== null) {
      flushCode()
      continue
    }
    if (codeLines !== null) {
      codeLines.push(rawLine)
      continue
    }

    // 表格收集与处理
    if (tableLines !== null) {
      if (line.includes('|') && line.trim()) {
        tableLines.push(line)
        continue
      } else {
        flushTable()
      }
    } else if (line.includes('|') && idx + 1 < lines.length && isDelimiterRow(lines[idx + 1])) {
      flushParagraph()
      flushList()
      tableLines = [line]
      continue
    }

    // 空行：结束当前段落与列表
    if (!line.trim()) {
      flushParagraph()
      flushList()
      flushTable()
      continue
    }

    // 分隔线
    if (/^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      flushParagraph()
      flushList()
      flushTable()
      blocks.push('<hr>')
      continue
    }

    // 标题：# 映射为 h4，## → h5，### 及更深 → h6
    const heading = /^(#{1,6})\s+(.*)$/.exec(line)
    if (heading) {
      flushParagraph()
      flushList()
      flushTable()
      const level = Math.min(6, heading[1].length + 3)
      blocks.push(`<h${level}>${renderInline(heading[2], inlineCodeClass)}</h${level}>`)
      continue
    }

    // 引用
    const quote = /^\s*>\s?(.*)$/.exec(line)
    if (quote) {
      flushParagraph()
      flushList()
      flushTable()
      blocks.push(`<blockquote>${renderInline(quote[1], inlineCodeClass)}</blockquote>`)
      continue
    }

    // 无序列表
    const bullet = /^\s*[-*+]\s+(.*)$/.exec(line)
    if (bullet) {
      flushParagraph()
      flushTable()
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
      flushTable()
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
    flushTable()
    paragraph.push(line.trim())
  }

  flushParagraph()
  flushList()
  flushTable()
  flushCode()

  return blocks.join('')
}
