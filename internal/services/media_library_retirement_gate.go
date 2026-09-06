package services

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func mediaLibraryRetiringTx(tx *gorm.DB, libraryID uint) (bool, error) {
	var id string
	err := tx.Model(&models.MediaLibraryRetirement{}).Select("id").Where("library_id=? AND phase<>?", libraryID, "completed").Limit(1).Scan(&id).Error
	return id != "", err
}

func requireMediaLibraryNotRetiringTx(tx *gorm.DB, libraryID uint) error {
	retiring, err := mediaLibraryRetiringTx(tx, libraryID)
	if err != nil {
		return err
	}
	if retiring {
		return appError(CodeConflict, "媒体库正在移除索引，请到任务中心查看进度", nil)
	}
	return nil
}

// scope is a service-owned SQL predicate over media_libraries, never user input.
// Configuration writers must not invalidate a retirement's frozen source/head.
// Keep this separate from physical drain: retirement itself calls that guard.
func requireCatalogScopeNotRetiringTx(tx *gorm.DB, scope string, id uint) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	var retirementID string
	if err := tx.Model(&models.MediaLibraryRetirement{}).Select("id").Where("phase<>? AND library_id IN (SELECT id FROM media_libraries WHERE "+scope+")", "completed", id).Limit(1).Scan(&retirementID).Error; err != nil {
		return err
	}
	if retirementID != "" {
		return appError(CodeConflict, "关联媒体库正在移除索引，请等待移除完成后再修改来源或规则", nil)
	}
	return nil
}

func mediaLibraryRetirementTx(tx *gorm.DB, libraryID uint) (*models.MediaLibraryRetirement, error) {
	var row models.MediaLibraryRetirement
	err := tx.Where("library_id=?", libraryID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}
