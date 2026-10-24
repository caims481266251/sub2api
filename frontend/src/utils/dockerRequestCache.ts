export const DOCKER_REQUEST_CACHE_KEY = 'docker_request_cache_enabled'
export function readDockerRequestCache(extra?: Record<string, unknown> | null): boolean {
  return extra?.[DOCKER_REQUEST_CACHE_KEY] !== false
}
