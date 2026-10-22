// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ProviderSynchronizationStatus from '@/components/ProviderSynchronizationStatus.vue'
import { emptyMediaLibraryDraft, payloadFromDraft, supportsSidecarUpload } from '@/media-libraries'
import type { StorageSummary } from '@/types/api'

describe('Quark library synchronization', () => {
  it('reports incomplete coverage and delayed estimates without claiming completion', () => {
    const wrapper = mount(ProviderSynchronizationStatus, { props: { enabled: true, synchronization: { mode: 'directory_observation', pending_directories: 12, missing_candidates: 2, last_complete_at: null, oldest_pending_at: '2026-10-20T00:00:00Z', estimated_coverage_at: '2026-10-22T00:00:00Z', estimate_is_lower_bound: true, cooldown_until: null, delayed: true } } })
    expect(wrapper.text()).toContain('覆盖延期')
    expect(wrapper.text()).toContain('不早于')
    expect(wrapper.text()).toContain('尚无记录')
    expect(wrapper.text()).toContain('待确认缺失')
    expect(wrapper.text()).toContain('12')
    wrapper.unmount()
  })
  it('does not infer cloud sidecar writeback or symlinks from general upload support', () => {
    const storage = { type: 'quark', capabilities: { small_file_upload: true, temporary_direct_url: true, signed_proxy: true } } as StorageSummary
    const draft = emptyMediaLibraryDraft(1, 1)
    expect(draft.transfer_mode).toBe('move'); expect(draft.conflict_policy).toBe('ask')
    draft.upload_sidecars = true; draft.transfer_mode = 'symlink'; draft.cloud_empty_cleanup_enabled = true
    expect(supportsSidecarUpload(storage)).toBe(false)
    expect(payloadFromDraft(draft, storage)).toMatchObject({ upload_sidecars: false, transfer_mode: 'move', cloud_empty_cleanup_enabled: false })
  })
})
