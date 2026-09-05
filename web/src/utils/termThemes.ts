// xterm.js color themes selectable in 终端偏好. The names must match
// prefs.Themes() on the server, which validates the stored value.
import type { ITheme } from '@xterm/xterm'

// GitHub Dark is the historical default and matches the console's own palette.
const githubDark: ITheme = {
  background: '#0d1117',
  foreground: '#c9d1d9',
  cursor: '#c9d1d9',
  selectionBackground: '#264f78',
  black: '#484f58',
  red: '#ff7b72',
  green: '#3fb950',
  yellow: '#d29922',
  blue: '#58a6ff',
  magenta: '#bc8cff',
  cyan: '#39c5cf',
  white: '#b1bac4',
  brightBlack: '#6e7681',
  brightRed: '#ffa198',
  brightGreen: '#56d364',
  brightYellow: '#e3b341',
  brightBlue: '#79c0ff',
  brightMagenta: '#d2a8ff',
  brightCyan: '#56d4dd',
  brightWhite: '#f0f6fc',
}

const dracula: ITheme = {
  background: '#282a36',
  foreground: '#f8f8f2',
  cursor: '#f8f8f2',
  selectionBackground: '#44475a',
  black: '#21222c',
  red: '#ff5555',
  green: '#50fa7b',
  yellow: '#f1fa8c',
  blue: '#bd93f9',
  magenta: '#ff79c6',
  cyan: '#8be9fd',
  white: '#f8f8f2',
  brightBlack: '#6272a4',
  brightRed: '#ff6e6e',
  brightGreen: '#69ff94',
  brightYellow: '#ffffa5',
  brightBlue: '#d6acff',
  brightMagenta: '#ff92df',
  brightCyan: '#a4ffff',
  brightWhite: '#ffffff',
}

// Miku: a light-on-teal palette named after the same-coloured accent.
const miku: ITheme = {
  background: '#101c1e',
  foreground: '#d6f7f3',
  cursor: '#39c5bb',
  selectionBackground: '#1f4d4a',
  black: '#1a2c2e',
  red: '#f2777a',
  green: '#39c5bb',
  yellow: '#ffcc66',
  blue: '#6699cc',
  magenta: '#cc99cc',
  cyan: '#66cccc',
  white: '#d6f7f3',
  brightBlack: '#4a6b6d',
  brightRed: '#ff9a9c',
  brightGreen: '#5fe0d6',
  brightYellow: '#ffe08a',
  brightBlue: '#8fb8e0',
  brightMagenta: '#e0b3e0',
  brightCyan: '#8fe0e0',
  brightWhite: '#f2fffd',
}

const solarizedDark: ITheme = {
  background: '#002b36',
  foreground: '#839496',
  cursor: '#93a1a1',
  selectionBackground: '#073642',
  black: '#073642',
  red: '#dc322f',
  green: '#859900',
  yellow: '#b58900',
  blue: '#268bd2',
  magenta: '#d33682',
  cyan: '#2aa198',
  white: '#eee8d5',
  brightBlack: '#586e75',
  brightRed: '#cb4b16',
  brightGreen: '#586e75',
  brightYellow: '#657b83',
  brightBlue: '#839496',
  brightMagenta: '#6c71c4',
  brightCyan: '#93a1a1',
  brightWhite: '#fdf6e3',
}

const monokai: ITheme = {
  background: '#272822',
  foreground: '#f8f8f2',
  cursor: '#f8f8f0',
  selectionBackground: '#49483e',
  black: '#272822',
  red: '#f92672',
  green: '#a6e22e',
  yellow: '#e6db74',
  blue: '#66d9ef',
  magenta: '#ae81ff',
  cyan: '#a1efe4',
  white: '#f8f8f2',
  brightBlack: '#75715e',
  brightRed: '#fd5ff0',
  brightGreen: '#cfff87',
  brightYellow: '#fff59d',
  brightBlue: '#9fd8ff',
  brightMagenta: '#d0aeff',
  brightCyan: '#c5f7f0',
  brightWhite: '#ffffff',
}

export const TERM_THEMES: Record<string, ITheme> = {
  'GitHub Dark': githubDark,
  Dracula: dracula,
  Miku: miku,
  'Solarized Dark': solarizedDark,
  Monokai: monokai,
}

export const DEFAULT_TERM_THEME = 'GitHub Dark'

// resolveTermTheme falls back to the default palette for an unknown name so a
// stale stored preference never leaves the terminal unstyled.
export function resolveTermTheme(name: string): ITheme {
  return TERM_THEMES[name] ?? TERM_THEMES[DEFAULT_TERM_THEME]
}
