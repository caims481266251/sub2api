import { computed, onBeforeUnmount, ref, watch, type Ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { DockerSystemPool } from '@/types'
import { DOCKER_SYSTEM_PROFILES, type DockerSystemSelection } from '@/utils/dockerSystemProfile'

export function useDockerSystemPool(open: Readonly<Ref<boolean>>, enabled: Readonly<Ref<boolean>>) {
  const selection = ref<DockerSystemSelection>('auto')
  const pool = ref<DockerSystemPool | null>(null)
  const loading = ref(false)
  const error = ref(false)
  let revision = 0

  const profiles = computed(() => DOCKER_SYSTEM_PROFILES.map(profile => {
    const observed = pool.value?.profiles.find(item => item.profile_id === profile.profile_id)
    return { ...profile, ...observed, available: observed?.available === true,
      bound_account_count: observed?.bound_account_count ?? null }
  }))
  const canSubmit = computed(() => !loading.value && !error.value && pool.value !== null &&
    (selection.value === 'auto' ? profiles.value.some(profile => profile.available) :
      profiles.value.some(profile => profile.profile_id === selection.value && profile.available)))

  async function refresh() {
    if (!open.value || !enabled.value) return
    const current = ++revision
    loading.value = true
    error.value = false
    try {
      const result = await adminAPI.accounts.getDockerSystemPool()
      if (current !== revision) return
      if (!Array.isArray(result.profiles) || !Number.isInteger(result.legacy_bound_account_count) ||
        result.legacy_bound_account_count < 0 || typeof result.checked_at !== 'string' ||
        !['all', 'visible'].includes(result.count_scope) ||
        result.profiles.some(profile => typeof profile.profile_id !== 'string' ||
          typeof profile.available !== 'boolean' || !Number.isInteger(profile.bound_account_count) || profile.bound_account_count < 0)) {
        throw new Error('invalid_docker_system_pool')
      }
      if (new Set(result.profiles.map(profile => profile.profile_id)).size !== result.profiles.length) {
        throw new Error('duplicate_docker_system_profile')
      }
      pool.value = result
    } catch {
      if (current === revision) error.value = true
    } finally {
      if (current === revision) loading.value = false
    }
  }

  watch([open, enabled], ([visible, docker], previous) => {
    if (!visible || !previous?.[0]) selection.value = 'auto'
    if (visible && docker) void refresh()
    else { revision++; loading.value = false; error.value = false; pool.value = null }
  }, { immediate: true })
  onBeforeUnmount(() => { revision++ })
  return { selection, pool, profiles, loading, error, canSubmit, refresh }
}
