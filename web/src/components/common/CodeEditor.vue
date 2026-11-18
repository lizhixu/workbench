<!-- CodeMirror 6 文本编辑器：文件管理的内置编辑器。
  行号 / 按文件名自动选语言高亮 / 查找替换（Ctrl+F）/ Tab 缩进 /
  Ctrl+S 保存 / 底部行列状态；主题跟随面板（深色 one-dark）。 -->
<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { Compartment, EditorState, type Extension } from '@codemirror/state'
import { EditorView, keymap, placeholder } from '@codemirror/view'
import { basicSetup } from 'codemirror'
import { indentWithTab } from '@codemirror/commands'
import { StreamLanguage } from '@codemirror/language'
import { oneDark } from '@codemirror/theme-one-dark'
import { useSettingsStore } from '../../stores/settings'

const props = defineProps<{
  modelValue: string
  /** 用于按扩展名选语言高亮（如 custom.cnf、nginx.conf、app.json）。 */
  filename?: string
  readonly?: boolean
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string]
  /** Ctrl+S / Cmd+S：父组件决定是否写入主机。 */
  save: []
  cursor: [pos: { line: number; col: number }]
  /** 语言高亮解析完成时回传展示名（如 JSON、Shell、纯文本）。 */
  language: [name: string]
}>()

const container = ref<HTMLDivElement | null>(null)
const settingsStore = useSettingsStore()

let view: EditorView | null = null
const languageConf = new Compartment()
const themeConf = new Compartment()

// 基础外观：等宽字体 13px / 行高 1.6（与旧文本框一致），背景用面板卡片色。
const baseTheme = EditorView.theme({
  '&': { fontSize: '13px' },
  '.cm-content': {
    fontFamily: "'JetBrains Mono', Consolas, 'Liberation Mono', Menlo, monospace",
    lineHeight: '1.6',
  },
  '.cm-gutters': { backgroundColor: 'transparent' },
  '&.cm-focused': { outline: 'none' },
})

type LegacyKind = 'shell' | 'toml' | 'nginx' | 'properties' | 'dockerFile' | 'ruby' | 'powerShell'

async function streamMode(kind: LegacyKind): Promise<Extension | null> {
  try {
    switch (kind) {
      case 'shell':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/shell')).shell)
      case 'toml':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/toml')).toml)
      case 'nginx':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/nginx')).nginx)
      case 'properties':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/properties')).properties)
      case 'dockerFile':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/dockerfile')).dockerFile)
      case 'ruby':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/ruby')).ruby)
      case 'powerShell':
        return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/powershell')).powerShell)
    }
  } catch {
    return null
  }
}

// 按文件名选语言：现代语言包按扩展名直取，ini/toml/shell/nginx 等走
// legacy-modes 的流式模式；无匹配时返回纯文本（ext 为空，仍有展示名）。
async function loadLanguage(filename?: string): Promise<{ ext: Extension | null; name: string }> {
  const lower = (filename || '').toLowerCase()
  const base = lower.split('/').pop() || lower
  const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : ''
  try {
    switch (ext) {
      case 'json':
        return { ext: (await import('@codemirror/lang-json')).json(), name: 'JSON' }
      case 'yaml':
      case 'yml':
        return { ext: (await import('@codemirror/lang-yaml')).yaml(), name: 'YAML' }
      case 'js':
      case 'mjs':
      case 'cjs':
        return { ext: (await import('@codemirror/lang-javascript')).javascript(), name: 'JavaScript' }
      case 'ts':
      case 'mts':
      case 'cts':
        return { ext: (await import('@codemirror/lang-javascript')).javascript({ typescript: true }), name: 'TypeScript' }
      case 'jsx':
        return { ext: (await import('@codemirror/lang-javascript')).javascript({ jsx: true }), name: 'JSX' }
      case 'tsx':
        return { ext: (await import('@codemirror/lang-javascript')).javascript({ typescript: true, jsx: true }), name: 'TSX' }
      case 'vue':
        return { ext: (await import('@codemirror/lang-html')).html(), name: 'Vue' }
      case 'html':
      case 'htm':
        return { ext: (await import('@codemirror/lang-html')).html(), name: 'HTML' }
      case 'css':
      case 'scss':
        return { ext: (await import('@codemirror/lang-css')).css(), name: 'CSS' }
      case 'xml':
        return { ext: (await import('@codemirror/lang-xml')).xml(), name: 'XML' }
      case 'sql':
        return { ext: (await import('@codemirror/lang-sql')).sql(), name: 'SQL' }
      case 'md':
      case 'markdown':
        return { ext: (await import('@codemirror/lang-markdown')).markdown(), name: 'Markdown' }
      case 'py':
        return { ext: (await import('@codemirror/lang-python')).python(), name: 'Python' }
      case 'go':
        return { ext: (await import('@codemirror/lang-go')).go(), name: 'Go' }
      case 'php':
        return { ext: (await import('@codemirror/lang-php')).php(), name: 'PHP' }
      case 'rs':
        return { ext: (await import('@codemirror/lang-rust')).rust(), name: 'Rust' }
      case 'java':
        return { ext: (await import('@codemirror/lang-java')).java(), name: 'Java' }
      case 'kt':
        return { ext: (await import('@codemirror/lang-java')).java(), name: 'Kotlin' }
      case 'c':
      case 'h':
      case 'cpp':
      case 'hpp':
        return { ext: (await import('@codemirror/lang-cpp')).cpp(), name: 'C++' }
      case 'sh':
      case 'bash':
      case 'zsh':
        return { ext: await streamMode('shell'), name: 'Shell' }
      case 'ps1':
        return { ext: await streamMode('powerShell'), name: 'PowerShell' }
      case 'toml':
        return { ext: await streamMode('toml'), name: 'TOML' }
      case 'cnf':
      case 'cfg':
      case 'ini':
      case 'env':
      case 'properties':
      case 'service':
        return { ext: await streamMode('properties'), name: 'INI' }
      case 'conf':
        // nginx 配置走 nginx 高亮，其余 .conf 按 ini 风格处理（如 my.cnf）。
        return lower.includes('nginx')
          ? { ext: await streamMode('nginx'), name: 'Nginx' }
          : { ext: await streamMode('properties'), name: 'INI' }
      case 'rb':
        return { ext: await streamMode('ruby'), name: 'Ruby' }
      default:
        break
    }
    if (base === 'dockerfile' || base.startsWith('dockerfile.')) {
      return { ext: await streamMode('dockerFile'), name: 'Dockerfile' }
    }
    return { ext: null, name: '纯文本' }
  } catch {
    // 语言包加载失败时退化为纯文本，不阻断编辑。
    return { ext: null, name: '纯文本' }
  }
}

function emitCursor() {
  if (!view) return
  const head = view.state.selection.main.head
  const line = view.state.doc.lineAt(head)
  emit('cursor', { line: line.number, col: head - line.from + 1 })
}

onMounted(() => {
  if (!container.value) return
  const state = EditorState.create({
    doc: props.modelValue,
    extensions: [
      basicSetup,
      languageConf.of([]),
      themeConf.of(settingsStore.themeMode === 'dark' ? oneDark : []),
      baseTheme,
      EditorView.lineWrapping,
      placeholder('空文件'),
      EditorState.readOnly.of(props.readonly ?? false),
      EditorView.updateListener.of((update) => {
        if (update.docChanged) emit('update:modelValue', update.state.doc.toString())
        if (update.selectionSet || update.docChanged) emitCursor()
      }),
      keymap.of([
        indentWithTab,
        {
          key: 'Mod-s',
          run: () => {
            emit('save')
            return true
          },
        },
      ]),
    ],
  })
  view = new EditorView({ state, parent: container.value })
  view.focus()
  loadLanguage(props.filename)
    .then(({ ext, name }) => {
      emit('language', name)
      if (view && ext) view.dispatch({ effects: languageConf.reconfigure(ext) })
    })
    .catch(() => {})
})

onUnmounted(() => {
  view?.destroy()
  view = null
})

// 父组件重设内容时（如重新打开另一个文件）同步进编辑器；
// 编辑器自身改动经 updateListener 回传，内容一致时不重复派发。
watch(
  () => props.modelValue,
  (next) => {
    if (!view) return
    const current = view.state.doc.toString()
    if (next !== current) {
      view.dispatch({ changes: { from: 0, to: current.length, insert: next } })
    }
  },
)

watch(
  () => props.filename,
  (name) => {
    loadLanguage(name)
      .then(({ ext, name: langName }) => {
        emit('language', langName)
        if (view) view.dispatch({ effects: languageConf.reconfigure(ext ?? []) })
      })
      .catch(() => {})
  },
)

watch(
  () => settingsStore.themeMode,
  (mode) => {
    view?.dispatch({ effects: themeConf.reconfigure(mode === 'dark' ? oneDark : []) })
  },
)
</script>

<template>
  <div ref="container" class="code-editor" />
</template>

<style scoped lang="scss">
.code-editor {
  height: 100%;
  min-height: 0;

  :deep(.cm-editor) {
    height: 100%;
    border: 1px solid var(--border-color);
    border-radius: 6px;
    background-color: var(--bg-card);
  }

  :deep(.cm-editor.cm-focused) {
    border-color: #6366f1;
  }

  :deep(.cm-scroller) {
    overflow: auto;
  }
}
</style>
