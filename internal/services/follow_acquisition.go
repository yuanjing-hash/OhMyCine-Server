package services

const (
	FollowAcquisitionTransferFirst = "transfer_first"
	FollowAcquisitionExistingOrder = "existing_order"
)

func validateAcquisitionPreference(value string, allowLegacy bool) error {
	if value == FollowAcquisitionTransferFirst || value == FollowAcquisitionExistingOrder || allowLegacy && value == "" {
		return nil
	}
	return appError(CodeFollowConfigurationInvalid, "资源获取顺序无效", nil)
}

func followAcquisitionTier(preference, source string) int {
	if preference == FollowAcquisitionTransferFirst && !isCloudShareSource(source) {
		return 1
	}
	return 0
}
