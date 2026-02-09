package controller

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

const PaymentMethodWallet = "wallet"

type SubscriptionWalletPayRequest struct {
	PlanId int `json:"plan_id"`
}

func SubscriptionRequestWalletPay(c *gin.Context) {
	var req SubscriptionWalletPayRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanId <= 0 {
		common.ApiErrorMsg(c, "参数错误")
		return
	}

	plan, err := model.GetSubscriptionPlanById(req.PlanId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !plan.Enabled {
		common.ApiErrorMsg(c, "套餐未启用")
		return
	}
	if plan.PriceAmount < 0 {
		common.ApiErrorMsg(c, "套餐金额不合法")
		return
	}

	userId := c.GetInt("id")
	if plan.MaxPurchasePerUser > 0 {
		count, err := model.CountUserSubscriptionsByPlan(userId, plan.Id)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			common.ApiErrorMsg(c, "已达到该套餐购买上限")
			return
		}
	}

	quotaCost, displayAmount, displayRate := calcSubscriptionWalletQuota(plan)
	tradeNo := fmt.Sprintf("SUBWALLET%dNO%s", userId, fmt.Sprintf("%s%d", common.GetRandomString(6), time.Now().Unix()))

	payload := map[string]any{
		"payment_method": PaymentMethodWallet,
		"quota_cost":     quotaCost,
		"display_amount": displayAmount,
		"display_rate":   displayRate,
		"display_type":   operation_setting.GetQuotaDisplayType(),
	}
	payloadStr := ""
	if payloadBytes, err := common.Marshal(payload); err == nil {
		payloadStr = string(payloadBytes)
	}

	if err := model.CompleteWalletSubscriptionOrder(tradeNo, userId, plan, PaymentMethodWallet, quotaCost, payloadStr); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}

	c.JSON(200, gin.H{
		"message": "success",
		"data": gin.H{
			"trade_no": tradeNo,
		},
	})
}

func calcSubscriptionWalletQuota(plan *model.SubscriptionPlan) (int, float64, float64) {
	rate := getSubscriptionWalletDisplayRate()
	if rate <= 0 {
		rate = 1
	}
	dRate := decimal.NewFromFloat(rate)
	dPrice := decimal.NewFromFloat(plan.PriceAmount)
	dDisplay := dPrice.Mul(dRate)

	usdAmount := dPrice
	if !dRate.IsZero() {
		usdAmount = dDisplay.Div(dRate)
	}
	quota := usdAmount.Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	return int(quota.IntPart()), dDisplay.InexactFloat64(), rate
}

func getSubscriptionWalletDisplayRate() float64 {
	generalSetting := operation_setting.GetGeneralSetting()
	switch generalSetting.QuotaDisplayType {
	case operation_setting.QuotaDisplayTypeUSD:
		return 1
	case operation_setting.QuotaDisplayTypeCNY:
		if operation_setting.USDExchangeRate > 0 {
			return operation_setting.USDExchangeRate
		}
		return 1
	case operation_setting.QuotaDisplayTypeCustom:
		if generalSetting.CustomCurrencyExchangeRate > 0 {
			return generalSetting.CustomCurrencyExchangeRate
		}
		return 1
	default:
		return 1
	}
}
