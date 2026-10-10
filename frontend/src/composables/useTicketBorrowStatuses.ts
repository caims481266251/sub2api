import { onBeforeUnmount, onMounted, ref } from 'vue'
import { ticketBorrowing, type BorrowTarget } from '@/api/admin/ticketBorrowing'
const rows = ref<BorrowTarget[]>([]), now = ref(Date.now()), offset = ref(0)
let users = 0, inflight = false, generation = 0
let timer: ReturnType<typeof setInterval> | undefined, clock: ReturnType<typeof setInterval> | undefined
async function refresh() {
  if (inflight) return
  inflight = true; const version = generation
  try { const result = await ticketBorrowing.targets(); if (users && generation === version) { rows.value = result; if (result[0]) offset.value = Date.parse(result[0].server_now) - Date.now() } }
  catch { /* A failed read never extends a previously observed expiry. */ }
  finally { inflight = false }
}
export function useTicketBorrowStatuses() {
  onMounted(() => { if (++users === 1) { generation++; void refresh(); timer = setInterval(refresh, 5000); clock = setInterval(() => { now.value = Date.now() }, 250) } })
  onBeforeUnmount(() => { if (--users === 0) { generation++; clearInterval(timer); clearInterval(clock); rows.value = [] } })
  return { rows, now, offset }
}
