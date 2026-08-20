/**
 * Safe clipboard copy utility with robust fallback for non-secure HTTP contexts.
 *
 * Navigator.clipboard.writeText is only available in Secure Contexts (HTTPS or
 * localhost). In plain HTTP ip/domain contexts it may exist but silently fail,
 * or be undefined entirely. We verify success and fall back to a legacy
 * textarea + document.execCommand('copy') technique, keeping the textarea
 * focused long enough for the command to commit.
 */

function legacyCopy(text: string): boolean {
  try {
    const textArea = document.createElement('textarea')
    textArea.value = text
    // Place outside the viewport but keep it rendered (display:none breaks select on iOS).
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
    textArea.style.opacity = '0'
    textArea.setAttribute('readonly', '')
    // Prevent zoom on iOS.
    textArea.style.fontSize = '12px'

    document.body.appendChild(textArea)

    // Save current focus so we can restore it afterwards.
    const previouslyFocused = document.activeElement as HTMLElement | null

    textArea.focus()
    textArea.select()
    textArea.setSelectionRange(0, text.length)

    let successful = false
    try {
      successful = document.execCommand('copy')
    } catch {
      successful = false
    }

    document.body.removeChild(textArea)

    // Restore focus to the original element (e.g. the button in the modal).
    if (previouslyFocused && typeof previouslyFocused.focus === 'function') {
      try {
        previouslyFocused.focus()
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

  // 1. Modern Clipboard API — only reliable in Secure Contexts.
  if (
    typeof navigator !== 'undefined' &&
    navigator.clipboard &&
    typeof navigator.clipboard.writeText === 'function' &&
    (window.isSecureContext === true ||
      window.location.protocol === 'https:' ||
      window.location.hostname === 'localhost' ||
      window.location.hostname === '127.0.0.1')
  ) {
    try {
      await navigator.clipboard.writeText(text)
      // Verify by reading back — some browsers silently fail write in HTTP.
      if (navigator.clipboard.readText) {
        try {
          const readBack = await navigator.clipboard.readText()
          if (readBack === text) return true
        } catch {
          // readText may be blocked; assume write succeeded.
          return true
        }
      }
      return true
    } catch {
      // fall through to legacy
    }
  }

  // 2. Legacy fallback for non-secure contexts.
  if (legacyCopy(text)) return true

  // 3. Final fallback — open a prompt so the user can manually copy.
  try {
    window.prompt('请手动复制以下内容（Ctrl+C / Cmd+C）：', text)
    return true
  } catch {
    return false
  }
}