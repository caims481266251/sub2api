import { computed, ref, watch, type Ref } from 'vue'

export interface BorrowEditorState<T> {
  draft: T
  baseline: string
}

// Cache values and their original revision separately from the latest server data.
// The page owns this small state, so unopened account forms need not stay mounted.
export function useBorrowEditorState<T>(initial: T, cached: BorrowEditorState<T> | undefined, update: (state: BorrowEditorState<T>) => void) {
  const draft = ref(cached?.draft ?? initial) as Ref<T>
  const baseline = ref(cached?.baseline ?? JSON.stringify(initial))
  const dirty = computed(() => JSON.stringify(draft.value) !== baseline.value)
  watch([draft, baseline], () => update({ draft: draft.value, baseline: baseline.value }), { deep: true, immediate: true, flush: 'sync' })
  return { draft, baseline, dirty }
}
