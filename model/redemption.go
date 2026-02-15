package model

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"

	"gorm.io/gorm"
)

// ErrRedeemFailed is returned when redemption fails due to database error
var ErrRedeemFailed = errors.New("redeem.failed")

type Redemption struct {
	Id            int            `json:"id"`
	UserId        int            `json:"user_id"`
	Key           string         `json:"key" gorm:"type:char(32);uniqueIndex"`
	Status        int            `json:"status" gorm:"default:1"`
	Name          string         `json:"name" gorm:"index"`
	Quota         int            `json:"quota" gorm:"default:100"`
	MaxUses       int            `json:"max_uses" gorm:"default:1"`
	UsedCount     int            `json:"used_count" gorm:"default:0"`
	CreatedTime   int64          `json:"created_time" gorm:"bigint"`
	RedeemedTime  int64          `json:"redeemed_time" gorm:"bigint"`
	Count         int            `json:"count" gorm:"-:all"` // only for api request
	UsedUserId    int            `json:"used_user_id"`
	DeletedAt     gorm.DeletedAt `gorm:"index"`
	ExpiredTime   int64          `json:"expired_time" gorm:"bigint"` // 过期时间，0 表示不过期
	RemainingUses int            `json:"remaining_uses" gorm:"-:all"`
}

type RedemptionUsage struct {
	Id           int            `json:"id"`
	RedemptionId int            `json:"redemption_id" gorm:"index:idx_redemption_user,unique"`
	UserId       int            `json:"user_id" gorm:"index:idx_redemption_user,unique"`
	RedeemedTime int64          `json:"redeemed_time" gorm:"bigint"`
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

func normalizeRedemptionUsage(redemption *Redemption) {
	if redemption == nil {
		return
	}
	if redemption.MaxUses <= 0 {
		redemption.MaxUses = 1
	}
	if redemption.UsedCount < 0 {
		redemption.UsedCount = 0
	}
	remaining := redemption.MaxUses - redemption.UsedCount
	if remaining < 0 {
		remaining = 0
	}
	redemption.RemainingUses = remaining
}

func normalizeRedemptionList(redemptions []*Redemption) {
	for _, redemption := range redemptions {
		normalizeRedemptionUsage(redemption)
	}
}

func GetAllRedemptions(startIdx int, num int) (redemptions []*Redemption, total int64, err error) {
	// 开始事务
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 获取总数
	err = tx.Model(&Redemption{}).Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// 获取分页数据
	err = tx.Order("id desc").Limit(num).Offset(startIdx).Find(&redemptions).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// 提交事务
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	normalizeRedemptionList(redemptions)
	return redemptions, total, nil
}

func SearchRedemptions(keyword string, startIdx int, num int) (redemptions []*Redemption, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Build query based on keyword type
	query := tx.Model(&Redemption{})

	// Only try to convert to ID if the string represents a valid integer
	if id, err := strconv.Atoi(keyword); err == nil {
		query = query.Where("id = ? OR name LIKE ?", id, keyword+"%")
	} else {
		query = query.Where("name LIKE ?", keyword+"%")
	}

	// Get total count
	err = query.Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Get paginated data
	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&redemptions).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	normalizeRedemptionList(redemptions)
	return redemptions, total, nil
}

func GetRedemptionById(id int) (*Redemption, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	redemption := Redemption{Id: id}
	var err error = nil
	err = DB.First(&redemption, "id = ?", id).Error
	if err == nil {
		normalizeRedemptionUsage(&redemption)
	}
	return &redemption, err
}

func Redeem(key string, userId int) (quota int, err error) {
	if key == "" {
		return 0, errors.New(i18n.MsgRedemptionNotProvided)
	}
	if userId == 0 {
		return 0, errors.New(i18n.MsgInvalidParams)
	}
	redemption := &Redemption{}

	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}
	common.RandomSleep()
	err = DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Set("gorm:query_option", "FOR UPDATE").Where(keyCol+" = ?", key).First(redemption).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New(i18n.MsgRedemptionInvalid)
			}
			return err
		}
		if redemption.Status == common.RedemptionCodeStatusDisabled {
			return errors.New(i18n.MsgRedemptionUsed)
		}
		if redemption.ExpiredTime != 0 && redemption.ExpiredTime < common.GetTimestamp() {
			return errors.New(i18n.MsgRedemptionExpired)
		}
		if redemption.MaxUses <= 0 {
			redemption.MaxUses = 1
		}
		if redemption.Status == common.RedemptionCodeStatusUsed || redemption.UsedCount >= redemption.MaxUses {
			return errors.New(i18n.MsgRedemptionUsed)
		}

		var usageCount int64
		err = tx.Model(&RedemptionUsage{}).Where("redemption_id = ? AND user_id = ?", redemption.Id, userId).Count(&usageCount).Error
		if err != nil {
			return err
		}
		if usageCount > 0 {
			return errors.New(i18n.MsgRedemptionUsed)
		}

		err = tx.Model(&User{}).Where("id = ?", userId).Update("quota", gorm.Expr("quota + ?", redemption.Quota)).Error
		if err != nil {
			return err
		}

		now := common.GetTimestamp()
		usage := RedemptionUsage{
			RedemptionId: redemption.Id,
			UserId:       userId,
			RedeemedTime: now,
		}
		if err = tx.Create(&usage).Error; err != nil {
			return err
		}

		redemption.RedeemedTime = now
		redemption.UsedUserId = userId
		redemption.UsedCount++
		if redemption.UsedCount >= redemption.MaxUses {
			redemption.Status = common.RedemptionCodeStatusUsed
		} else {
			redemption.Status = common.RedemptionCodeStatusEnabled
		}
		err = tx.Model(redemption).Select("redeemed_time", "status", "used_user_id", "used_count").Updates(redemption).Error
		return err
	})
	if err != nil {
		if err.Error() == i18n.MsgRedemptionInvalid || err.Error() == i18n.MsgRedemptionUsed || err.Error() == i18n.MsgRedemptionExpired || err.Error() == i18n.MsgRedemptionNotProvided {
			return 0, err
		}
		common.SysError("redemption failed: " + err.Error())
		return 0, ErrRedeemFailed
	}
	RecordLog(userId, LogTypeTopup, fmt.Sprintf("通过兑换码充值 %s，兑换码ID %d", logger.LogQuota(redemption.Quota), redemption.Id))
	return redemption.Quota, nil
}

func (redemption *Redemption) Insert() error {
	var err error
	err = DB.Create(redemption).Error
	return err
}

func (redemption *Redemption) SelectUpdate() error {
	// This can update zero values
	return DB.Model(redemption).Select("redeemed_time", "status").Updates(redemption).Error
}

// Update Make sure your token's fields is completed, because this will update non-zero values
func (redemption *Redemption) Update() error {
	var err error
	err = DB.Model(redemption).Select("name", "status", "quota", "max_uses", "redeemed_time", "expired_time").Updates(redemption).Error
	return err
}

func (redemption *Redemption) Delete() error {
	var err error
	err = DB.Delete(redemption).Error
	return err
}

func DeleteRedemptionById(id int) (err error) {
	if id == 0 {
		return errors.New("id 为空！")
	}
	redemption := Redemption{Id: id}
	err = DB.Where(redemption).First(&redemption).Error
	if err != nil {
		return err
	}
	if err = DB.Where("redemption_id = ?", redemption.Id).Delete(&RedemptionUsage{}).Error; err != nil {
		return err
	}
	return redemption.Delete()
}

func BatchDeleteRedemptions(ids []int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var rowsAffected int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("redemption_id IN ?", ids).Delete(&RedemptionUsage{}).Error; err != nil {
			return err
		}
		result := tx.Where("id IN ?", ids).Delete(&Redemption{})
		if result.Error != nil {
			return result.Error
		}
		rowsAffected = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

func DeleteInvalidRedemptions() (int64, error) {
	now := common.GetTimestamp()
	var rowsAffected int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var ids []int
		err := tx.Model(&Redemption{}).
			Where("status IN ? OR (status = ? AND expired_time != 0 AND expired_time < ?)", []int{common.RedemptionCodeStatusUsed, common.RedemptionCodeStatusDisabled}, common.RedemptionCodeStatusEnabled, now).
			Pluck("id", &ids).Error
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			rowsAffected = 0
			return nil
		}
		if err := tx.Where("redemption_id IN ?", ids).Delete(&RedemptionUsage{}).Error; err != nil {
			return err
		}
		result := tx.Where("id IN ?", ids).Delete(&Redemption{})
		if result.Error != nil {
			return result.Error
		}
		rowsAffected = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}
