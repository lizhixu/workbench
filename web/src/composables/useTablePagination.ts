// Shared table pagination helper.
//
// AGENTS.md 8.2 requires every unbounded data list to keep its header and
// pagination bar fixed while only the data area scrolls. For tables whose rows
// are already in memory, naive-ui's built-in `pagination` prop does the slicing
// and renders the bar inside the table wrapper (below the scroll body), which is
// exactly that layout — so views only need this one reactive config object
// instead of hand-rolling page state, slicing, and clamping each time.
//
// Server-paged tables (audit logs, session records) keep their own `offset` /
// `page_size` requests plus a standalone NPagination in `.table-pagination-bar`;
// this helper is for the client-side case.
import { reactive, watch, type Ref } from 'vue'

export const DEFAULT_PAGE_SIZES = [20, 50, 100]

export interface TablePaginationOptions {
  pageSize?: number
  pageSizes?: number[]
  /**
   * Row count source. When provided, the current page is clamped as rows
   * disappear (delete, filter change), so the table never renders blank while
   * records still exist.
   */
  rowCount?: Ref<number>
}

/**
 * Creates a reactive pagination config for `<NDataTable :pagination="...">`.
 * Pass the table the full row array; naive-ui slices it per page.
 */
export function useTablePagination(opts: TablePaginationOptions = {}) {
  const pagination = reactive({
    page: 1,
    pageSize: opts.pageSize ?? 20,
    showSizePicker: true,
    pageSizes: opts.pageSizes ?? DEFAULT_PAGE_SIZES,
    onChange: (page: number) => {
      pagination.page = page
    },
    onUpdatePageSize: (pageSize: number) => {
      pagination.pageSize = pageSize
      pagination.page = 1
    },
  })

  if (opts.rowCount) {
    watch([opts.rowCount, () => pagination.pageSize], ([count, size]) => {
      const maxPage = Math.max(1, Math.ceil((count as number) / (size as number)))
      if (pagination.page > maxPage) pagination.page = maxPage
    })
  }

  /** Resets to page 1 — call when a filter or the data source changes. */
  function resetPage() {
    pagination.page = 1
  }

  return { pagination, resetPage }
}
