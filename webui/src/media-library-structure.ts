import type { MediaLibraryStructureBulkSelection, MediaLibraryStructureDiagnostics, MediaLibraryStructureIssueSummary, MediaLibraryStructureSelectionAction, MediaLibraryStructureReviewSummary } from '@/types/api'

export function structureReviewSummary(value: MediaLibraryStructureReviewSummary): MediaLibraryStructureReviewSummary {
  const count = (v: unknown) => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0
  const keys = ['unrecognized', 'missing_season_episode', 'naming_mismatch', 'location_mismatch', 'invalid_path', 'template_unavailable', 'duplicate_target', 'recognition_suspect_conflict', 'catalog_duplicate_conflict', 'sidecar_target_conflict'] as const
  if (!value || typeof value.diagnosis_revision !== 'string' || !value.diagnosis_revision || ![value.review_revision, value.pending_total, value.handled_total, value.pending_repairable_count].every(count) || value.pending_repairable_count > value.pending_total || ![value.pending_classifications, value.handled_classifications].every(classes => classes && keys.every(key => count(classes[key])))) {
    throw new Error('本次处理统计暂不可用，请刷新后重试；不能据此判断没有问题。')
  }
  return { diagnosis_revision: value.diagnosis_revision, review_revision: value.review_revision, pending_total: value.pending_total, handled_total: value.handled_total, pending_repairable_count: value.pending_repairable_count, pending_classifications: value.pending_classifications, handled_classifications: value.handled_classifications }
}

export type StructureIssueAction = MediaLibraryStructureSelectionAction | 'review_recognition' | 'rescan_catalog' | 'manual_recognition' | 'edit_rules'

const physicalConflictCodes = ['duplicate_target', 'sidecar_target_conflict']
const reviewConflictCodes = ['recognition_suspect_conflict', 'catalog_duplicate_conflict']

export function structureIssueActions(issue: MediaLibraryStructureIssueSummary): StructureIssueAction[] {
  // Identity/index uncertainty must take priority over generic repair flags.
  if (issue.code === 'recognition_suspect_conflict') return ['review_recognition', 'skip']
  if (issue.code === 'catalog_duplicate_conflict') return ['rescan_catalog', 'skip']
  if (physicalConflictCodes.includes(issue.code)) {
    if (issue.members.length < 2) return ['skip']
    const actions: StructureIssueAction[] = ['keep_member', 'keep_all_versions', 'skip']
    if (issue.members.some(member => member.token === issue.recommended_member_token)) actions.unshift('keep_recommended')
    return actions
  }
  if (issue.repairable || issue.state === 'manual_identity_resolved') return ['repair', 'skip']
  if (issue.code === 'media_unrecognized') return ['manual_recognition', 'skip']
  if (issue.code === 'template_unavailable') return ['edit_rules', 'skip']
  return ['skip']
}

export function structureBulkConflictCodes(action: MediaLibraryStructureBulkSelection['action'], currentCode = ''): string[] {
  if (action === 'skip' && currentCode && currentCode !== 'all' && currentCode !== 'missing_season_episode') return [currentCode]
	const codes = action === 'keep_recommended' ? physicalConflictCodes : [...physicalConflictCodes, ...reviewConflictCodes]
  return currentCode ? codes.filter(code => code === currentCode) : [...codes]
}

export function structureNeedsDecisionCount(diagnostics: Pick<MediaLibraryStructureDiagnostics, 'issue_count' | 'repairable_count'> | null): number {
  if (!diagnostics) return 0
  return Math.max(0, diagnostics.issue_count - diagnostics.repairable_count)
}
