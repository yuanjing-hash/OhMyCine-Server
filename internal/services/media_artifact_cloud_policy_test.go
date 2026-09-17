package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestCloudCleanupDisabledSupersedesMixedLocalPolicy(t *testing.T) {
	library := models.MediaLibrary{StorageID: 1, ArtifactGeneration: 1, Enabled: true, STRMEnabled: true, SignedProxyEnabled: true, STRMLocalRoot: "X:/projection"}
	policy := mediaArtifactPolicy{StorageID: 1, Generation: 1, TargetKind: models.MediaArtifactTargetLocalProjection, ProjectionRoot: "X:/projection", CloudEmptyCleanupEnabled: true}
	if artifactPolicyMatchesLibrary(policy, library) {
		t.Fatal("disabled cloud flag left mixed cloud execution resumable")
	}
	policy.CloudEmptyCleanupEnabled = false
	if !artifactPolicyMatchesLibrary(policy, library) {
		t.Fatal("disabled flag disturbed ordinary local policy")
	}
}
