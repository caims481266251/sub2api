import { describe, it, expect } from 'vitest'
import { borrowAttemptDetails } from './borrowAttemptDetails'
describe('borrowAttemptDetails', () => {
  it('does not infer route attribution from historical account errors', () => {
    for (const raw of [null, '', '{', '{}', '[null,{}, {"account_id":63}]']) expect(borrowAttemptDetails(raw)).toEqual([])
  })
  it('keeps distinct attempts and the latest snapshot for a stream failure', () => {
    const attempt = { kind: 'egress', borrow_route_id: 'route-a', number: 1, account_id: 63, events: {} }
    const rows = borrowAttemptDetails(JSON.stringify([
      { attempt }, { attempt: { ...attempt, number: 3, borrow_route_id: 'route-b' } },
      { attempt: { ...attempt, number: 3, borrow_route_id: 'route-b', no_text_long_tail: true }, borrow_recovery: { stop_reason: 'attempt_limit' } }
    ]))
    expect(rows).toHaveLength(2)
    expect(rows[0].attempt.borrow_route_id).toBe('route-a')
    expect(rows[1].attempt.no_text_long_tail).toBe(true)
    expect(rows[1].recovery?.stop_reason).toBe('attempt_limit')
  })
})
