import { describe, expect, it } from 'vitest'
import { attemptStageSpans } from '../requestTimingStages'
import type { TimingAttempt } from '@/api/admin/usageTiming'

describe('transport stage intervals', () => {
  it('preserves missing phases on a warm connection and separates upstream TTFT', () => {
    const attempt = { number: 2, start_ms: 100, events: { connection_start: 110, connection_ready: 112, request_written: 120, first_byte: 150, first_semantic: 200 } } as TimingAttempt
    const spans = attemptStageSpans(attempt)
    expect(spans.map(s => s.name)).toEqual(['connection', 'request_write', 'wait_first_byte', 'upstream_first_semantic'])
    expect(spans.find(s => s.name === 'upstream_first_semantic')).toMatchObject({ start_ms: 120, end_ms: 200 })
  })
  it('retains zero-length measured stages and ignores incomplete or reversed observations', () => {
    const attempt = { number: 1, events: { proxy_tls_start: 0, proxy_tls_end: 0, proxy_connect_start: 5, proxy_connect_end: 4, tls_start: 10 } } as TimingAttempt
    expect(attemptStageSpans(attempt)).toEqual([{ name: 'proxy_tls', start_ms: 0, end_ms: 0, attempt: 1 }])
  })
})
