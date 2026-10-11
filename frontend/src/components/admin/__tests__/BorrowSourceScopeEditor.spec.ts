import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import BorrowSourceScopeEditor from '../BorrowSourceScopeEditor.vue'
import Select from '@/components/common/Select.vue'
import { changeSourceScope, includedInScope } from '@/composables/borrowSourceScope'
import type { BorrowSelection, BorrowSourceOption } from '@/api/admin/ticketBorrowing'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, args?: Record<string, unknown>) => key + (args ? JSON.stringify(args) : '') }) }))
const config = (): BorrowSelection => ({ revision: 3, group_ids: [], account_ids: [], excluded_account_ids: [], default_model: 'test-model', rotate_identity: false, model_overrides: { 1: 'custom-model' } })
const account = (id: number, groups: number[] = [2, 3]): BorrowSourceOption => ({ id, name: `Account ${id}`, group_ids: groups, status: 'active', schedulable: true, request_eligible: true, is_target: false })

describe('source scope rules', () => {
  it('excludes accounts across all selected groups and restores dynamic membership without discarding models', () => {
    const accounts = [account(1), account(2)]
    const initial = { ...config(), group_ids: [2, 3], account_ids: [1] }
    const removed = changeSourceScope(initial, accounts, [1, 1], false)
    expect(removed.account_ids).toEqual([])
    expect(removed.excluded_account_ids).toEqual([1])
    expect(includedInScope(accounts[0], removed)).toBe(false)
    expect(includedInScope(accounts[1], removed)).toBe(true)
    expect(includedInScope(account(3), removed)).toBe(true)
    expect(removed.model_overrides).toEqual({ 1: 'custom-model' })
    const restored = changeSourceScope(removed, accounts, [1], true)
    expect(restored.account_ids).toEqual([])
    expect(restored.excluded_account_ids).toEqual([])
    expect(includedInScope(accounts[0], restored)).toBe(true)
    expect(initial.account_ids).toEqual([1])
  })
  it('removes explicit-only accounts without adding a future exclusion, and skips target additions', () => {
    const accounts = [account(1, []), { ...account(2), is_target: true }]
    const removed = changeSourceScope({ ...config(), account_ids: [1] }, accounts, [1], false)
    expect(removed.account_ids).toEqual([])
    expect(removed.excluded_account_ids).toEqual([])
    expect(changeSourceScope(removed, accounts, [1, 2, 999], true).account_ids).toEqual([1])
  })
})

describe('source scope editor', () => {
  const mountEditor = (selection = config(), accounts = Array.from({ length: 25 }, (_, i) => account(i + 1))) => mount(BorrowSourceScopeEditor, {
    props: { modelValue: selection, accounts, groups: [{ id: 2, name: 'Group' }], disabled: false }
  })
  it('selects pages independently, freezes all-filtered IDs and does not mutate configuration on checking', async () => {
    const w = mountEditor()
    await w.get('[data-testid="scope-select-page"]').setValue(true)
    expect(w.emitted('change')).toBeUndefined()
    expect(w.get('[data-testid="scope-add"]').text()).toContain('"count":20')
    await w.findAll('button').find(b => b.text() === 'pagination.next')!.trigger('click')
    await w.get('[data-testid="scope-select-page"]').setValue(true)
    await w.get('[data-testid="scope-select-page"]').setValue(false)
    expect(w.get('[data-testid="scope-add"]').text()).toContain('"count":20')
    await w.get('[data-testid="scope-select-matching"]').trigger('click')
    await w.setProps({ accounts: Array.from({ length: 26 }, (_, i) => account(i + 1)) })
    await w.get('[data-testid="scope-add"]').trigger('click')
    expect((w.emitted('change')!.at(-1)![0] as BorrowSelection).account_ids).toEqual(Array.from({ length: 25 }, (_, i) => i + 1))
    w.unmount()
  })
  it('clears operation checks on filtering and uses exact numeric IDs', async () => {
    const w = mountEditor()
    await w.get('[data-testid="scope-select-matching"]').trigger('click')
    await w.get('[data-testid="scope-search"]').setValue('#2')
    expect(w.findAll('[data-scope-row]')).toHaveLength(1)
    expect(w.get('[data-scope-row]').attributes('data-scope-row')).toBe('2')
    expect(w.get('[data-testid="scope-add"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('ticketBorrowing.management.selectionCleared')
    expect(w.emitted('change')).toBeUndefined()
    w.unmount()
  })
  it('bulk restores excluded members, including restoration after leaving a group', async () => {
    const w = mountEditor({ ...config(), group_ids: [2], excluded_account_ids: [1, 2] }, [account(1), account(2, [])])
    w.findAllComponents(Select).find(c => c.props('ariaLabel') === 'ticketBorrowing.scope.status')!.vm.$emit('update:modelValue', 'excluded')
    await w.vm.$nextTick()
    await w.get('[data-testid="scope-select-matching"]').trigger('click')
    await w.get('[data-testid="scope-add"]').trigger('click')
    expect(w.emitted('change')!.at(-1)![0]).toEqual(expect.objectContaining({ account_ids: [2], excluded_account_ids: [], group_ids: [2] }))
    w.unmount()
  })
  it('bulk removes unavailable and target members and cleans missing references', async () => {
    const w = mountEditor({ ...config(), group_ids: [2, 999], account_ids: [999], excluded_account_ids: [998] }, [{ ...account(1), is_target: true }, { ...account(2), request_eligible: false }])
    await w.get('[data-testid="scope-select-matching"]').trigger('click')
    await w.get('[data-testid="scope-remove"]').trigger('click')
    expect(w.emitted('change')!.at(-1)![0]).toEqual(expect.objectContaining({ excluded_account_ids: [1, 2, 998], group_ids: [2, 999] }))
    await w.get('[data-testid="scope-clean-missing"]').trigger('click')
    expect(w.emitted('change')!.at(-1)![0]).toEqual(expect.objectContaining({ account_ids: [], excluded_account_ids: [], group_ids: [2] }))
    w.unmount()
  })
})
