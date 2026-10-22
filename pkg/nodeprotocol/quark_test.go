package nodeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuarkPlansRequireDigestV2AndUnsupportedReplaceIsRejected(t *testing.T) {
	p := validStoragePlanForTest()
	p.ProviderType = StorageProviderQuark
	if p.Validate() == nil {
		t.Fatal("legacy digest accepted")
	}
	p.DigestVersion = 2
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	p.Files[0].ConflictAction = StorageConflictReplace
	if p.Validate() == nil {
		t.Fatal("unverified recycle/replace accepted")
	}
	p = validStoragePlanForTest()
	if e := p.Validate(); e != nil {
		t.Fatal("legacy115 changed", e)
	}
	caps := Capabilities{Codes: []string{CapabilityPan115Upload, CapabilityPan115Read, CapabilityPan115ShareReceive, CapabilityRangeExport}}
	if QuarkUploadSupported(caps) || QuarkShareSupported(caps) {
		t.Fatal("old node accepted")
	}
	caps.Codes = append(caps.Codes, CapabilityQuarkReadV2, CapabilityQuarkShareV2, CapabilityQuarkUploadV2, CapabilityContentDigestV2)
	if !QuarkUploadSupported(caps) || !QuarkShareSupported(caps) {
		t.Fatal("complete v2 capabilities rejected")
	}
	d := ContentDigest{Version: 2, Algorithm: "sha256", Value: strings.Repeat("a", 64), VerifiedBy: "full_readback"}
	if !d.ValidReadbackSHA256(d.Value) {
		t.Fatal("readback digest rejected")
	}
	d.VerifiedBy = "provider_metadata"
	if d.ValidReadbackSHA256(d.Value) {
		t.Fatal("unverified provider digest accepted")
	}
}

func TestQuarkDirectoryLocatorsAreFrozenAndLegacyRejectsThem(t *testing.T) {
	source := StorageSourcePlan{DigestVersion: 2, TaskDirectoryName: "omc-task", StorageID: "storage-1", ProviderType: StorageProviderQuark, SourceKind: StorageSourceKindQuarkShareSelected, TargetKind: StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("a", 64), SourceContentDigest: strings.Repeat("b", 64), OfflineDestinationID: "intake", DestinationRelativePath: "/nested/intake"}
	upload := validStoragePlanForTest()
	upload.ProviderType, upload.DigestVersion, upload.TargetRootRelativePath = StorageProviderQuark, 2, "/nested/library"
	if err := source.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := upload.Validate(); err != nil {
		t.Fatal(err)
	}
	digest := func(value any) string {
		raw, _ := json.Marshal(value)
		d, err := (OperationPlan{Payload: raw}).Digest()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	beforeSource, beforeUpload := digest(source), digest(upload)
	source.DestinationRelativePath = "/other"
	upload.TargetRootRelativePath = "/other"
	if beforeSource == digest(source) || beforeUpload == digest(upload) {
		t.Fatal("locator change did not alter immutable plan digest")
	}
	for _, invalid := range []string{"relative", "/../outside", "/nested/../other", "//other", "/nested\\other", "/nested/", "/nested\nother"} {
		source.DestinationRelativePath, upload.TargetRootRelativePath = invalid, invalid
		if source.Validate() == nil || upload.Validate() == nil {
			t.Fatalf("unsafe locator accepted: %q", invalid)
		}
	}
	source.ProviderType, source.SourceKind, source.DigestVersion, source.TaskDirectoryName, source.DestinationRelativePath = StorageProviderPan115, StorageSourceKindPan115Share, 0, "", "/"
	upload.ProviderType, upload.DigestVersion, upload.TargetRootRelativePath = StorageProviderPan115, 0, "/"
	if source.Validate() == nil || upload.Validate() == nil {
		t.Fatal("legacy115 accepted Quark locator fields")
	}
	source.DestinationRelativePath, upload.TargetRootRelativePath = "", ""
	if source.Validate() != nil || upload.Validate() != nil {
		t.Fatal("legacy115 compatibility changed")
	}
}
