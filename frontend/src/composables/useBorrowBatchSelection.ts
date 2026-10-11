import { computed, ref, watch, type Ref } from 'vue'

export function useBorrowBatchSelection(pageIds: Ref<number[]>, filteredIds: Ref<number[]>) {
  const selected = ref<number[]>([])
  const allPage = computed(() => pageIds.value.length > 0 && pageIds.value.every(id => selected.value.includes(id)))
  const somePage = computed(() => pageIds.value.some(id => selected.value.includes(id)))
  function toggle(ids: number[], checked: boolean) {
    const scope = new Set(ids)
    selected.value = checked ? [...new Set([...selected.value, ...ids])] : selected.value.filter(id => !scope.has(id))
  }
  // Refreshes may remove obsolete rows but never add newly matching accounts.
  watch(filteredIds, ids => {
    const valid = new Set(ids)
    selected.value = selected.value.filter(id => valid.has(id))
  })
  return { selected, allPage, somePage, toggle }
}
