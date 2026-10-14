import type { TimingAttempt, TimingSpan } from '@/api/admin/usageTiming'

// These intervals overlap. Missing observations stay absent, including TLS
// on a reused connection. SOCKS includes dialing the proxy, not only CONNECT.
export function attemptStageSpans(attempt: TimingAttempt): TimingSpan[] {
  const pairs = [
    ['client_acquire', 'client_acquire_start', 'client_acquire_end'],
    ['connection', 'connection_start', 'connection_ready'],
    ['dns', 'dns_start', 'dns_end'],
    ['tcp', 'tcp_start', 'tcp_end'],
    ['proxy_tls', 'proxy_tls_start', 'proxy_tls_end'],
    ['proxy_connect', 'proxy_connect_start', 'proxy_connect_end'],
    ['socks_setup', 'socks_setup_start', 'socks_setup_end'],
    ['tls', 'tls_start', 'tls_end'],
    ['request_write', 'connection_ready', 'request_written'],
    ['wait_first_byte', 'request_written', 'first_byte'],
    ['upstream_first_semantic', 'request_written', 'first_semantic'],
    ['upstream_first_visible', 'request_written', 'first_visible']
  ]
  return pairs.flatMap(([name, start, end]) => {
    const a = attempt.events[start], b = attempt.events[end]
    return Number.isFinite(a) && Number.isFinite(b) && b >= a
      ? [{ name, start_ms: a, end_ms: b, attempt: attempt.number }] : []
  })
}
