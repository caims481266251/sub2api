import type { BorrowTarget } from '@/api/admin/ticketBorrowing'
import { borrowReady } from './borrowStatus'

export interface BorrowAvailability { sampledMS: number; missingMS: number; lastAt: number; missing: boolean; enabled: boolean }

// This is explicitly a page-local sampling estimate, never reconstructed
// historical uptime. Hidden tabs, failed polls and gaps over 10s are unknown.
export function observeBorrowAvailability(previous: BorrowAvailability | undefined, target: BorrowTarget, now: number): BorrowAvailability {
  const elapsed = previous ? now - previous.lastAt : 0
  const covered = previous?.enabled && target.enabled && elapsed > 0 && elapsed <= 10000 ? elapsed : 0
  const missing = !borrowReady(target, now)
  return { sampledMS: (previous?.sampledMS ?? 0) + covered, missingMS: (previous?.missingMS ?? 0) + (previous?.missing && missing ? covered : 0), lastAt: now, missing, enabled: target.enabled }
}
