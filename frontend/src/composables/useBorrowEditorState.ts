import { computed, ref, toValue, watch, type MaybeRefOrGetter, type Ref } from 'vue'

export interface BorrowEditorState<T> {
  draft: T
  baseline: string
}

// Cache values and their original revision separately from the latest server data.
// The page owns this small state, so unopened account forms need not stay mounted.
export function useBorrowEditorState<T>(initial: T, cached: MaybeRefOrGetter<BorrowEditorState<T> | undefined>, update: (state: BorrowEditorState<T>) => void) {
  const previous = toValue(cached)
  const keepDraft = previous && JSON.stringify(previous.draft) !== previous.baseline
  const draft = ref(keepDraft ? previous.draft : initial) as Ref<T>
  const baseline = ref(keepDraft ? previous.baseline : JSON.stringify(initial))
  const dirty = computed(() => JSON.stringify(draft.value) !== baseline.value)
  let receiving = false
  watch([draft, baseline], () => {
    if (!receiving) update({ draft: draft.value, baseline: baseline.value })
  }, { deep: true, immediate: true, flush: 'sync' })
  // A page-owned save may complete after the editor was closed and reopened.
  watch(() => toValue(cached), state => {
    if (!state || (state.draft === draft.value && state.baseline === baseline.value)) return
    receiving = true
    draft.value = state.draft
    baseline.value = state.baseline
    receiving = false
  }, { flush: 'sync' })
  return { draft, baseline, dirty }
}
