import type { BorrowSelection, BorrowSourceOption } from '@/api/admin/ticketBorrowing'

export function includedByGroup(account: BorrowSourceOption, selection: BorrowSelection) {
  return account.group_ids.some(id => selection.group_ids.includes(id))
}

export function includedInScope(account: BorrowSourceOption, selection: BorrowSelection) {
  return !(selection.excluded_account_ids ?? []).includes(account.id)
    && (selection.account_ids.includes(account.id) || includedByGroup(account, selection))
}

// These changes affect configuration intent, independently of transient health.
export function changeSourceScope(selection: BorrowSelection, accounts: BorrowSourceOption[], ids: number[], add: boolean): BorrowSelection {
  const result = { ...selection }
  const explicit = new Set(selection.account_ids)
  const excluded = new Set(selection.excluded_account_ids ?? [])
  const index = new Map(accounts.map(account => [account.id, account]))
  for (const id of ids) {
    const account = index.get(id)
    if (add) {
      if (!account || account.is_target) continue
      excluded.delete(id)
      if (!includedByGroup(account, selection)) explicit.add(id)
    } else {
      explicit.delete(id)
      if (account && includedByGroup(account, selection)) excluded.add(id)
    }
  }
  result.account_ids = [...explicit].filter(id => !excluded.has(id)).sort((a, b) => a - b)
  result.excluded_account_ids = [...excluded].sort((a, b) => a - b)
  return result
}

export function scopeWithinLimits(selection: BorrowSelection) {
  return [selection.group_ids, selection.account_ids, selection.excluded_account_ids ?? []].every(ids => ids.length <= 1000)
}
