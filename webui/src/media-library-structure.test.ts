import { describe, expect, it } from 'vitest'
import { structureBulkConflictCodes, structureIssueActions, structureNeedsDecisionCount } from '@/media-library-structure'
import type { MediaLibraryStructureIssueSummary } from '@/types/api'

function issue(code: string, overrides: Partial<MediaLibraryStructureIssueSummary> = {}): MediaLibraryStructureIssueSummary {
  return {
    token: 'issue-1', code, kind: 'video', state: 'needs_attention', repairable: false,
    recommended_member_token: 'member-1',
    members: [
      { token: 'member-1', source_path: '剧集/Season 02/01.mp4', recommended: true },
      { token: 'member-2', source_path: '剧集/Season 02/01 (1).mp4', recommended: false },
    ],
    ...overrides,
  }
}

describe('structure issue action policy', () => {
  it.each([
    ['recognition_suspect_conflict', 'review_recognition'],
    ['catalog_duplicate_conflict', 'rescan_catalog'],
  ])('keeps %s non-destructive even when generic repair flags are stale', (code, action) => {
    for (const overrides of [{}, { repairable: true }, { state: 'manual_identity_resolved' }, { members: [] }]) {
      expect(structureIssueActions(issue(code, overrides))).toEqual([action, 'skip'])
    }
  })

  it.each(['duplicate_target', 'sidecar_target_conflict'])('offers all physical retention choices only for %s', code => {
    expect(structureIssueActions(issue(code))).toEqual(['keep_recommended', 'keep_member', 'keep_all_versions', 'skip'])
    expect(structureIssueActions(issue(code, { recommended_member_token: undefined }))).toEqual(['keep_member', 'keep_all_versions', 'skip'])
    expect(structureIssueActions(issue(code, { recommended_member_token: 'missing-member' }))).not.toContain('keep_recommended')
    expect(structureIssueActions(issue(code, { members: [], repairable: true }))).toEqual(['skip'])
  })

  it('offers repair after manual identity save only outside unresolved conflicts', () => {
    expect(structureIssueActions(issue('media_unrecognized', { members: [] }))).toEqual(['manual_recognition', 'skip'])
    expect(structureIssueActions(issue('media_unrecognized', { state: 'manual_identity_resolved' }))).toEqual(['repair', 'skip'])
    expect(structureIssueActions(issue('path_mismatch', { repairable: true }))).toEqual(['repair', 'skip'])
    expect(structureIssueActions(issue('template_unavailable'))).toEqual(['edit_rules', 'skip'])
    expect(structureIssueActions(issue('invalid_path'))).toEqual(['skip'])
  })

  it('does not interpret an unfamiliar multi-member issue as physical copies', () => {
    expect(structureIssueActions(issue('future_conflict_type'))).toEqual(['skip'])
  })
})

describe('cross-page conflict drafts', () => {
  it('recommends only physical conflicts across the full paginated result', () => {
    expect(structureBulkConflictCodes('keep_recommended')).toEqual(['duplicate_target', 'sidecar_target_conflict'])
  })

  it('allows bulk skip of both review conflicts and physical conflicts', () => {
    expect(structureBulkConflictCodes('skip')).toEqual(['duplicate_target', 'sidecar_target_conflict', 'recognition_suspect_conflict', 'catalog_duplicate_conflict'])
  })

  it('restricts per-type submissions to actions valid for that type', () => {
    expect(structureBulkConflictCodes('keep_recommended', 'recognition_suspect_conflict')).toEqual([])
    expect(structureBulkConflictCodes('keep_recommended', 'catalog_duplicate_conflict')).toEqual([])
    expect(structureBulkConflictCodes('keep_recommended', 'path_mismatch')).toEqual([])
    expect(structureBulkConflictCodes('keep_recommended', 'duplicate_target')).toEqual(['duplicate_target'])
    expect(structureBulkConflictCodes('skip', 'catalog_duplicate_conflict')).toEqual(['catalog_duplicate_conflict'])
  })
})

describe('authoritative diagnosis totals', () => {
  it('counts undecided grouped issues separately from repairable issues', () => {
    expect(structureNeedsDecisionCount({ issue_count: 260, repairable_count: 210 })).toBe(50)
  })

  it('converges to zero after successful repair and does not require a paged list total', () => {
    expect(structureNeedsDecisionCount({ issue_count: 2, repairable_count: 0 })).toBe(2)
    expect(structureNeedsDecisionCount({ issue_count: 0, repairable_count: 0 })).toBe(0)
    expect(structureNeedsDecisionCount(null)).toBe(0)
  })
})
