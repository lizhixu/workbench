// Text/binary sniffing for the built-in file editor, following the same
// content-based rule git and VS Code use: the decision comes from the file's
// bytes, never from its name. The extension is only used to pick the
// editor's syntax highlighting mode (see CodeEditor).
//
// Rules, in order:
// 1. Empty input is text (an empty file is editable).
// 2. A UTF-8 BOM marks text; the BOM itself is skipped for the checks below.
// 3. A UTF-16 BOM (FF FE / FE FF) marks binary for our purposes: the editor
//    reads and writes UTF-8 only, so opening UTF-16 would show mojibake and
//    corrupt the file on save. Same trade-off git makes.
// 4. Any NUL byte in the sample marks binary.
// 5. Otherwise the sample must decode as strict UTF-8; undecodable bytes
//    mark binary (this also excludes GBK-encoded files for now — they would
//    already render as mojibake in the UTF-8 editor).

export const TEXT_SNIFF_BYTES = 8192

export function isTextContent(bytes: Uint8Array): boolean {
  let start = 0
  if (
    bytes.length >= 3 &&
    bytes[0] === 0xef &&
    bytes[1] === 0xbb &&
    bytes[2] === 0xbf
  ) {
    // UTF-8 BOM: text, skip the marker for the checks below.
    start = 3
  } else if (
    bytes.length >= 2 &&
    ((bytes[0] === 0xff && bytes[1] === 0xfe) ||
      (bytes[0] === 0xfe && bytes[1] === 0xff))
  ) {
    return false
  }
  for (let i = start; i < bytes.length; i++) {
    if (bytes[i] === 0) return false
  }
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(bytes.subarray(start))
    return true
  } catch {
    return false
  }
}
