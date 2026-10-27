import { ref } from 'vue'
import { createSharedComposable, useEventListener, useIntervalFn } from '@vueuse/core'

// All visible account rows share one display-only clock; no network requests.
export const useQualityDisplayClock = createSharedComposable(() => {
  const now = ref(Date.now())
  const update = () => { now.value = Date.now() }
  const { pause, resume } = useIntervalFn(update, 60_000)
  const syncVisibility = () => {
    if (document.hidden) pause()
    else { update(); resume() }
  }
  useEventListener(document, 'visibilitychange', syncVisibility)
  syncVisibility()
  return now
})
