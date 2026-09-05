import type { MediaLibraryStructureBulkSelection, MediaLibraryStructureDiagnostics, MediaLibraryStructureIssueSummary, MediaLibraryStructureSelectionAction } from '@/types/api'

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
  const codes = action === 'keep_recommended' ? physicalConflictCodes : [...physicalConflictCodes, ...reviewConflictCodes]
  return currentCode ? codes.filter(code => code === currentCode) : [...codes]
}

export function structureNeedsDecisionCount(diagnostics: Pick<MediaLibraryStructureDiagnostics, 'issue_count' | 'repairable_count'> | null): number {
  if (!diagnostics) return 0
  return Math.max(0, diagnostics.issue_count - diagnostics.repairable_count)
}
