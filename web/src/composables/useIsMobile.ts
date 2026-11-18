// Reactive narrow-screen flag, shared by views that hide secondary table
// columns or switch pagination to its compact form on mobile.
//
// Breakpoint 768px matches the global mobile rules in
// `web/src/styles/global.scss` and `AppShell.vue` (and DESIGN.md §9.4):
// keep the three in sync when the breakpoint ever changes.
import { onScopeDispose, ref, type Ref } from 'vue'

export const MOBILE_BREAKPOINT = '(max-width: 768px)'

export function useIsMobile(): Ref<boolean> {
  const isMobile = ref(false)
  if (typeof window === 'undefined' || !window.matchMedia) return isMobile

  const query = window.matchMedia(MOBILE_BREAKPOINT)
  isMobile.value = query.matches
  const onChange = (e: MediaQueryListEvent) => {
    isMobile.value = e.matches
  }
  query.addEventListener('change', onChange)
  onScopeDispose(() => query.removeEventListener('change', onChange))
  return isMobile
}
