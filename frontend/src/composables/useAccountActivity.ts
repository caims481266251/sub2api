import { computed, onBeforeUnmount, onMounted, ref, watch, type Ref } from 'vue'
import { getAccountActivity, type AccountActivity, type AccountQualityMonitor } from '@/api/admin/accountActivity'

type ActivityAccount = { id: number; platform: string; updated_at?: string }

// One page-wide poll; account rows never create their own timer or HTTP request.
export function useAccountActivity(accounts: Ref<readonly ActivityAccount[]>) {
  const byAccount = ref<Record<number, AccountActivity>>({})
  const stale = ref(false)
  const loading = ref(false)
  const accountIDs = computed(() => [...new Set(accounts.value.filter(row => row.platform === 'openai').map(row => row.id))])
  const accountKey = computed(() => accounts.value.filter(row => row.platform === 'openai').map(row => `${row.id}:${row.updated_at ?? ''}`).join(','))
  let active = false
  let generation = 0
  let controller: AbortController | null = null
  let timer: ReturnType<typeof setInterval> | undefined

  function cancelRead() {
    generation++
    controller?.abort()
    controller = null
    loading.value = false
  }

  async function refresh() {
    if (!active || document.hidden || controller || accountIDs.value.length === 0) return
    const read = new AbortController()
    controller = read
    loading.value = true
    const version = generation
    const ids = [...accountIDs.value]
    try {
      // The API caps batches at 200; large table pages are split sequentially.
      const items: AccountActivity[] = []
      for (let offset = 0; offset < ids.length; offset += 200) {
        items.push(...await getAccountActivity(ids.slice(offset, offset + 200), read.signal))
        if (!active || read.signal.aborted || generation !== version) return
      }
      const visible = new Set(ids)
      byAccount.value = Object.fromEntries(items.filter(item => visible.has(item.account_id)).map(item => [item.account_id, item]))
      stale.value = false
    } catch {
      if (active && !read.signal.aborted && generation === version) stale.value = true
    } finally {
      if (controller === read) {
        controller = null
        loading.value = false
      }
    }
  }

  function setQuality(quality: AccountQualityMonitor) {
    const current = byAccount.value[quality.account_id]
    if (current) byAccount.value = { ...byAccount.value, [quality.account_id]: { ...current, quality } }
    // An older poll must not replace the result of a save/run action.
    cancelRead()
    void refresh()
  }

  watch(accountKey, () => {
    cancelRead()
    const visible = new Set(accountIDs.value)
    byAccount.value = Object.fromEntries(Object.entries(byAccount.value).filter(([id]) => visible.has(Number(id))))
    stale.value = false
    void refresh()
  })

  function visibilityChanged() {
    if (document.hidden) {
      cancelRead()
      clearInterval(timer)
      timer = undefined
    } else {
      if (!timer) timer = setInterval(() => { void refresh() }, 5000)
      void refresh()
    }
  }

  onMounted(() => {
    active = true
    document.addEventListener('visibilitychange', visibilityChanged)
    visibilityChanged()
  })
  onBeforeUnmount(() => {
    active = false
    cancelRead()
    clearInterval(timer)
    document.removeEventListener('visibilitychange', visibilityChanged)
  })
  return { byAccount, stale, loading, refresh, setQuality }
}
