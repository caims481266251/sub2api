import { ref, watch, type Ref } from 'vue'

export function useBorrowPreference<T extends string | number>(key: string, initial: T): Ref<T> {
  const name = `borrow-management:${key}`
  const state = ref(initial) as Ref<T>
  try {
    const saved = JSON.parse(sessionStorage.getItem(name) ?? 'null')
    if (typeof saved === typeof initial && (typeof saved === 'string' ? saved.length <= 256 : Number.isFinite(saved) && saved >= 0)) state.value = saved
  } catch { /* Storage is optional, including in private browsing. */ }
  watch(state, value => { try { sessionStorage.setItem(name, JSON.stringify(value)) } catch { /* Keep the current UI usable. */ } })
  return state
}
