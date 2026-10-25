<template>
  <section class="card space-y-3 p-5">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div><h2 class="font-semibold">{{ t('ticketBorrowing.title') }}</h2><p class="mt-1 text-sm text-gray-500">{{ t('ticketBorrowing.legacyHint') }}</p></div>
      <RouterLink to="/admin/ticket-borrowing" class="btn btn-primary">{{ t('ticketBorrowing.title') }}</RouterLink>
    </div>
    <button v-if="legacy?.cookie_pool.enabled" class="btn btn-secondary" :disabled="busy" @click="stop">{{ t('ticketBorrowing.legacyStop') }}</button>
    <p v-if="message" role="status" class="text-sm text-gray-500">{{ message }}</p>
  </section>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getAstraGateway, saveAstraGateway, type AstraGatewaySettings } from '@/api/admin/astraGateway'
const {t}=useI18n()
const legacy=ref<AstraGatewaySettings>(),busy=ref(false),message=ref('')
onMounted(async()=>{try{legacy.value=await getAstraGateway()}catch{message.value=t('ticketBorrowing.failure')}})
async function stop(){if(!legacy.value||busy.value)return;busy.value=true;try{legacy.value=await saveAstraGateway({...legacy.value,cookie_pool:{...legacy.value.cookie_pool,enabled:false}});message.value=t('ticketBorrowing.legacyStopped')}catch{message.value=t('ticketBorrowing.failure')}finally{busy.value=false}}
</script>
