export const DOCKER_SYSTEM_PROFILES = [
  { profile_id: 'centos-stream-9', display_name: 'CentOS Stream 9' },
  { profile_id: 'ubuntu-24.04', display_name: 'Ubuntu 24.04' },
  { profile_id: 'debian-12', display_name: 'Debian 12' },
  { profile_id: 'archlinux', display_name: 'Arch Linux' },
  { profile_id: 'fedora-44', display_name: 'Fedora 44' },
] as const

export type DockerSystemSelection = 'auto' | typeof DOCKER_SYSTEM_PROFILES[number]['profile_id']

export function dockerSystemProfileName(profile: string): string {
  return DOCKER_SYSTEM_PROFILES.find(item => item.profile_id === profile)?.display_name ?? profile
}

// Auto is resolved per account by the backend, including batch imports.
export function withDockerSystemProfile<T extends { execution_mode?: 'normal' | 'docker_kernel'; docker_system_profile?: string }>(
  payload: T, selection: DockerSystemSelection,
): T {
  const result = { ...payload }
  if (result.execution_mode === 'docker_kernel') result.docker_system_profile = selection
  else delete result.docker_system_profile
  return result
}
