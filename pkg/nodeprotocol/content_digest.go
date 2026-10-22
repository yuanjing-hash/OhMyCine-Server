package nodeprotocol

// ContentDigest v2 distinguishes provider metadata from an actual complete
// readback. Legacy115 responses retain SHA1 and their existing interpretation.
type ContentDigest struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	Value      string `json:"value"`
	VerifiedBy string `json:"verified_by"`
}

func (d ContentDigest) ValidReadbackSHA256(expected string) bool {
	return d.Version == 2 && d.Algorithm == "sha256" && d.VerifiedBy == "full_readback" && ValidDigest(d.Value) && d.Value == expected
}

func QuarkUploadSupported(c Capabilities) bool {
	return c.Has(CapabilityQuarkUploadV2) && c.Has(CapabilityContentDigestV2) && c.Has(CapabilityRangeExport)
}
func QuarkShareSupported(c Capabilities) bool {
	return c.Has(CapabilityQuarkReadV2) && c.Has(CapabilityQuarkShareV2) && c.Has(CapabilityContentDigestV2) && c.Has(CapabilityRangeExport)
}
