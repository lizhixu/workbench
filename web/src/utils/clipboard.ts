/**
 * Universal clipboard copy utility with multi-tier fallback.
 * Works seamlessly in both HTTPS and plain HTTP IP environments,
 * and handles modal focus traps (e.g. Naive UI NModal / Element Plus dialogs).
 */

function copyViaEvent(text: string): boolean {
  let success = false
  const listener = (e: ClipboardEvent) => {
    if (e.clipboardData) {
      e.clipboardData.clearData()
      e.clipboardData.setData('text/plain', text)
      e.preventDefault()
      success = true
    }
  }

  try {
    document.addEventListener('copy', listener)
    // Trigger the copy event while user interaction is active
    document.execCommand('copy')
  } catch {
    success = false
  } finally {
    document.removeEventListener('copy', listener)
  }

  return success
}

function copyViaTextarea(text: string): boolean {
  try {
    const activeEl = (document.activeElement as HTMLElement) || document.body
    // Prefer appending inside active modal/container to avoid focus trap conflicts
    const container = activeEl.closest('.n-modal, .n-dialog, .n-drawer, [role="dialog"]') || document.body

    const textArea = document.createElement('textarea')
    textArea.value = text
    textArea.style.position = 'fixed'
    textArea.style.top = '0'
    textArea.style.left = '0'
    textArea.style.width = '2em'
    textArea.style.height = '2em'
    textArea.style.padding = '0'
    textArea.style.border = 'none'
    textArea.style.outline = 'none'
    textArea.style.boxShadow = 'none'
    textArea.style.background = 'transparent'
    textArea.style.opacity = '0.01'
    textArea.style.zIndex = '99999'

    container.appendChild(textArea)

    textArea.focus()
    textArea.select()
    textArea.setSelectionRange(0, text.length)

    let successful = false
    try {
      successful = document.execCommand('copy')
    } catch {
      successful = false
    }

    container.removeChild(textArea)

    if (activeEl && typeof activeEl.focus === 'function') {
      try {
        activeEl.focus()
      } catch {
        /* ignore */
      }
    }

    return successful
  } catch {
    return false
  }
}

export async function copyToClipboard(text: string): Promise<boolean> {
  if (!text) return false

  // 1. Try modern navigator.clipboard.writeText if available
  if (
    typeof navigator !== 'undefined' &&
    navigator.clipboard &&
    typeof navigator.clipboard.writeText === 'function'
  ) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Permission denied or non-secure context restriction; fallback to next strategies
    }
  }

  // 2. Try clipboard event interception (foolproof for user gesture in modals/HTTP)
  if (copyViaEvent(text)) {
    return true
  }

  // 3. Try in-container textarea selection
  if (copyViaTextarea(text)) {
    return true
  }

  // 4. Final fallback: prompt user to copy manually
  try {
    window.prompt('请手动复制以下内容（Ctrl+C / Cmd+C）：', text)
    return true
  } catch {
    return false
  }
}
