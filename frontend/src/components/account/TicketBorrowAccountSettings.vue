<template>
  <section class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
    <label class="flex items-center gap-3 font-medium">
      <input class="h-4 w-4 shrink-0 accent-primary-600" type="checkbox" :checked="status?.enabled ?? false" :disabled="busy || !status" @change="toggle" />
      {{ t('ticketBorrowing.enableBorrow') }}
    </label>
    <p class="text-xs text-gray-500">{{ t('ticketBorrowing.targetHint') }}</p>
    <p v-if="status?.enabled" class="break-words text-sm" :class="usable ? 'text-emerald-600' : 'text-amber-600'">
      {{ t(usable ? 'ticketBorrowing.ready' : 'ticketBorrowing.waiting') }}
      <span v-if="usable" class="mt-1 block break-all text-xs text-gray-500">{{ status.gateway }} · {{ t('ticketBorrowing.remaining', { time: remaining }) }}</span>
    </p>
    <p v-if="status?.enabled" class="text-xs text-gray-500">{{ t('ticketBorrowing.targetScope') }}</p>
    <p v-if="status?.enabled" class="text-xs text-gray-500">{{ t('ticketBorrowing.routeHint') }}</p>
    <BorrowProbePolicy v-if="status?.enabled" :key="accountId" :target="status" @saved="accept" />
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <button v-if="error" type="button" class="btn btn-secondary btn-sm" @click="load">{{ t('ticketBorrowing.refresh') }}</button>
  </section>
</template>
<script setup lang="ts">
import BorrowProbePolicy from '@/components/admin/BorrowProbePolicy.vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { ticketBorrowing, type BorrowTarget } from '@/api/admin/ticketBorrowing'
const props = defineProps<{ accountId: number }>()
const { t, te } = useI18n()
const status = ref<BorrowTarget>()
const busy = ref(false)
const error = ref('')
const now = ref(Date.now())
let serverOffset = 0
let generation = 0
let disposed = false
const ttl = computed(() => status.value?.expires_at ? Date.parse(status.value.expires_at) - now.value - serverOffset : 0)
const usable = computed(() => !!status.value?.ready && Date.parse(status.value.usable_until ?? status.value.expires_at ?? '') > now.value + serverOffset)
const remaining = computed(() => ttl.value < 1000 ? t('ticketBorrowing.underSecond') : t('ticketBorrowing.seconds', { count: Math.ceil(ttl.value / 1000) }))
function accept(value: BorrowTarget) { if (status.value?.account_id === value.account_id && (status.value?.probe_policy?.revision ?? 0) > (value.probe_policy?.revision ?? 0)) value = { ...value, probe_policy: status.value!.probe_policy }; serverOffset = Date.parse(value.server_now) - Date.now(); status.value = value; now.value = Date.now() }
function message(e: unknown) { const value = e as { reason?: string; message?: string }; const key = `ticketBorrowing.reasons.${value.reason || value.message}`; return te(key) ? t(key) : t('ticketBorrowing.failure') }
async function load() {
  const version = ++generation
  try { const value = await ticketBorrowing.target(props.accountId); if (!disposed && version === generation) { accept(value); error.value = '' } }
  catch (e) { if (!disposed && version === generation) error.value = message(e) }
}
async function toggle(event:Event) {
  if (!status.value || busy.value) return
  busy.value = true; error.value = ''; const version = ++generation
  try { const value = await ticketBorrowing.setTarget(props.accountId, !status.value.enabled, status.value.revision); if (!disposed && version === generation) accept(value) }
  catch (e) { if (!disposed && version === generation) error.value = message(e) }
  finally { busy.value = false; (event.target as HTMLInputElement).checked = status.value?.enabled ?? false }
}
watch(() => props.accountId, () => { status.value = undefined; void load() }, { immediate: true })
const clock = setInterval(() => { now.value = Date.now() }, 250)
const poll = setInterval(() => { if (!busy.value) void load() }, 5000)
onBeforeUnmount(() => { disposed = true; generation++; clearInterval(clock); clearInterval(poll) })
</script>
