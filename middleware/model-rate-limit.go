package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/limiter"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

const (
	ModelRequestRateLimitCountMark        = "MRRL"
	ModelRequestRateLimitSuccessCountMark = "MRRLS"
)

func newModelRateLimitRedisContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), common.RateLimitRedisOpTimeout)
}

func checkAndRecordSuccessRequest(rdb *redis.Client, key string, maxCount int, durationSeconds int64, durationMinutes int, entry string) (bool, error) {
	if maxCount == 0 {
		return true, nil
	}
	ctx, cancel := newModelRateLimitRedisContext()
	defer cancel()
	lim := limiter.New(ctx, rdb)
	expireSeconds := int64(time.Duration(durationMinutes) * time.Minute / time.Second)
	return lim.SlidingWindowWithEntry(ctx, key, maxCount, durationSeconds, expireSeconds, limiter.SlidingWindowModeCheckAndRecord, entry)
}

func rollbackSuccessRequest(rdb *redis.Client, key string, durationMinutes int, entry string) error {
	if entry == "" {
		return nil
	}
	ctx, cancel := newModelRateLimitRedisContext()
	defer cancel()
	lim := limiter.New(ctx, rdb)
	expireSeconds := int64(time.Duration(durationMinutes) * time.Minute / time.Second)
	_, err := lim.SlidingWindowWithEntry(ctx, key, 1, 1, expireSeconds, limiter.SlidingWindowModeRollback, entry)
	if err != nil {
		return err
	}
	// 可能已被并发请求挤出窗口或提前过期；按“尽力回滚”处理，不视为错误
	return nil
}

func rollbackSuccessRequestWithRetry(rdb *redis.Client, key string, durationMinutes int, entry string) {
	if err := rollbackSuccessRequest(rdb, key, durationMinutes, entry); err != nil {
		common.SysLog(fmt.Sprintf("rollback success request failed (first attempt), key=%s, entry=%s, err=%v", key, entry, err))
		if retryErr := rollbackSuccessRequest(rdb, key, durationMinutes, entry); retryErr != nil {
			common.SysLog(fmt.Sprintf("rollback success request failed (retry), key=%s, entry=%s, err=%v", key, entry, retryErr))
		}
	}
}

type modelRateLimitPolicy struct {
	Identifier      string
	DurationMinutes int
	TotalMaxCount   int
	SuccessMaxCount int
}

type redisSuccessRecord struct {
	successKey      string
	durationMinutes int
	entrySuffix     string
}

type memorySuccessRecord struct {
	successKey string
	maxCount   int
	duration   int64
}

func checkSingleRedisRateLimit(rdb *redis.Client, policy modelRateLimitPolicy) (bool, string, *redisSuccessRecord, error) {
	duration := int64(policy.DurationMinutes * 60)
	if duration <= 0 {
		return true, "", nil, nil
	}

	shard := common.HashShard(policy.Identifier, common.RateLimitKeyShardCount)
	successKey := fmt.Sprintf("rateLimit:model:%s:id:%s:%s", ModelRequestRateLimitSuccessCountMark, policy.Identifier, shard)
	requestEntrySuffix := ""

	if policy.SuccessMaxCount > 0 {
		requestEntrySuffix = common.GetUUID()
		allowed, err := checkAndRecordSuccessRequest(rdb, successKey, policy.SuccessMaxCount, duration, policy.DurationMinutes, requestEntrySuffix)
		if err != nil {
			return false, "", nil, err
		}
		if !allowed {
			return false, fmt.Sprintf("您已达到请求数限制：%d分钟内最多请求%d次", policy.DurationMinutes, policy.SuccessMaxCount), nil, nil
		}
	}

	if policy.TotalMaxCount > 0 {
		totalKey := fmt.Sprintf("rateLimit:model:%s:id:%s:%s", ModelRequestRateLimitCountMark, policy.Identifier, shard)
		ctx, cancel := newModelRateLimitRedisContext()
		tb := limiter.New(ctx, rdb)
		allowed, err := tb.Allow(
			ctx,
			totalKey,
			limiter.WithCapacity(int64(policy.TotalMaxCount)*duration),
			limiter.WithRate(int64(policy.TotalMaxCount)),
			limiter.WithRequested(duration),
			limiter.WithExpireSeconds(duration+60),
		)
		cancel()
		if err != nil {
			if requestEntrySuffix != "" {
				rollbackSuccessRequestWithRetry(rdb, successKey, policy.DurationMinutes, requestEntrySuffix)
			}
			return false, "", nil, err
		}
		if !allowed {
			if requestEntrySuffix != "" {
				rollbackSuccessRequestWithRetry(rdb, successKey, policy.DurationMinutes, requestEntrySuffix)
			}
			return false, fmt.Sprintf("您已达到总请求数限制：%d分钟内最多请求%d次，包括失败次数，请检查您的请求是否正确", policy.DurationMinutes, policy.TotalMaxCount), nil, nil
		}
	}

	if requestEntrySuffix != "" {
		return true, "", &redisSuccessRecord{
			successKey:      successKey,
			durationMinutes: policy.DurationMinutes,
			entrySuffix:     requestEntrySuffix,
		}, nil
	}
	return true, "", nil, nil
}

func enforceRedisModelRateLimit(c *gin.Context, policies []modelRateLimitPolicy) {
	rdb := common.RDB
	records := make([]redisSuccessRecord, 0)

	rollbackAll := func() {
		for i := range records {
			record := records[i]
			rollbackSuccessRequestWithRetry(rdb, record.successKey, record.durationMinutes, record.entrySuffix)
		}
	}

	for i := range policies {
		allowed, msg, record, err := checkSingleRedisRateLimit(rdb, policies[i])
		if err != nil {
			rollbackAll()
			fmt.Println("检查请求数限制失败:", err.Error())
			abortWithOpenAiMessage(c, http.StatusInternalServerError, "rate_limit_check_failed")
			return
		}
		if !allowed {
			rollbackAll()
			abortWithOpenAiMessage(c, http.StatusTooManyRequests, msg)
			return
		}
		if record != nil {
			records = append(records, *record)
		}
	}

	c.Next()

	if c.Writer.Status() >= 400 {
		rollbackAll()
	}
}

func enforceMemoryModelRateLimit(c *gin.Context, policies []modelRateLimitPolicy) {
	maxDurationMinutes := 1
	for i := range policies {
		if policies[i].DurationMinutes > maxDurationMinutes {
			maxDurationMinutes = policies[i].DurationMinutes
		}
	}
	inMemoryRateLimiter.Init(time.Duration(maxDurationMinutes) * time.Minute)

	successRecords := make([]memorySuccessRecord, 0)
	for i := range policies {
		policy := policies[i]
		duration := int64(policy.DurationMinutes * 60)
		if duration <= 0 {
			continue
		}
		totalKey := ModelRequestRateLimitCountMark + policy.Identifier
		successKey := ModelRequestRateLimitSuccessCountMark + policy.Identifier
		if !inMemoryRateLimiter.AllowWithCheck(totalKey, policy.TotalMaxCount, successKey, policy.SuccessMaxCount, duration) {
			c.Status(http.StatusTooManyRequests)
			c.Abort()
			return
		}
		if policy.SuccessMaxCount > 0 {
			successRecords = append(successRecords, memorySuccessRecord{
				successKey: successKey,
				maxCount:   policy.SuccessMaxCount,
				duration:   duration,
			})
		}
	}

	c.Next()

	if c.Writer.Status() < 400 {
		for i := range successRecords {
			record := successRecords[i]
			inMemoryRateLimiter.Request(record.successKey, record.maxCount, record.duration)
		}
	}
}

func appendPolicyIfHasLimit(policies []modelRateLimitPolicy, policy modelRateLimitPolicy) []modelRateLimitPolicy {
	if policy.DurationMinutes <= 0 {
		return policies
	}
	if policy.TotalMaxCount <= 0 && policy.SuccessMaxCount <= 0 {
		return policies
	}
	return append(policies, policy)
}

// ModelRequestRateLimit 模型请求限流中间件
func ModelRequestRateLimit() func(c *gin.Context) {
	return func(c *gin.Context) {
		// 在每个请求时检查是否启用限流
		systemEnabled := setting.ModelRequestRateLimitEnabled
		tokenRateLimitEnabled := common.GetContextKeyBool(c, constant.ContextKeyTokenRateLimitEnabled)
		ipEnabled := setting.ModelRequestIPRateLimitEnabled
		if !systemEnabled && !tokenRateLimitEnabled && !ipEnabled {
			c.Next()
			return
		}

		// 获取分组（用于分组配置以及 IP-Group 限制）
		group := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUserGroup)
		}

		policies := make([]modelRateLimitPolicy, 0, 4)

		// ------------------------------
		// 1) 现有模型请求限流（系统 + 令牌：取更严格限制）
		// ------------------------------
		systemDurationMinutes := 0
		systemTotalMaxCount := 0
		systemSuccessMaxCount := 0
		if systemEnabled {
			systemDurationMinutes = setting.ModelRequestRateLimitDurationMinutes
			systemTotalMaxCount = setting.ModelRequestRateLimitCount
			systemSuccessMaxCount = setting.ModelRequestRateLimitSuccessCount
			// 分组覆盖
			systemGroupTotalCount, systemGroupSuccessCount, found := setting.GetGroupRateLimit(group)
			if found {
				systemTotalMaxCount = systemGroupTotalCount
				systemSuccessMaxCount = systemGroupSuccessCount
			}
		}

		tokenDurationMinutes := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitDurationMins)
		tokenTotalMaxCount := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitCount)
		tokenSuccessMaxCount := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitSuccessCount)

		durationMinutes := systemDurationMinutes
		totalMaxCount := systemTotalMaxCount
		successMaxCount := systemSuccessMaxCount
		hasBaseLimit := totalMaxCount > 0 || successMaxCount > 0

		if tokenRateLimitEnabled {
			// 时长取较小值（更严格），允许 tokenDurationMinutes 为 0 时仅采用系统配置
			if tokenDurationMinutes > 0 && (durationMinutes == 0 || tokenDurationMinutes < durationMinutes) {
				durationMinutes = tokenDurationMinutes
			}
			// 计数取较小的正数（更严格），0 表示不限制
			if tokenTotalMaxCount > 0 && (totalMaxCount == 0 || tokenTotalMaxCount < totalMaxCount) {
				totalMaxCount = tokenTotalMaxCount
			}
			if tokenSuccessMaxCount > 0 && (successMaxCount == 0 || tokenSuccessMaxCount < successMaxCount) {
				successMaxCount = tokenSuccessMaxCount
			}
			hasBaseLimit = hasBaseLimit || tokenTotalMaxCount > 0 || tokenSuccessMaxCount > 0
		}

		// 标识符：优先 tokenId（保持现有行为），否则 userId
		baseIdentifier := strconv.Itoa(common.GetContextKeyInt(c, constant.ContextKeyTokenId))
		if baseIdentifier == "0" {
			baseIdentifier = strconv.Itoa(common.GetContextKeyInt(c, constant.ContextKeyUserId))
		}
		if baseIdentifier == "0" {
			baseIdentifier = strconv.Itoa(c.GetInt("id"))
		}

		if hasBaseLimit {
			policies = appendPolicyIfHasLimit(policies, modelRateLimitPolicy{
				Identifier:      baseIdentifier,
				DurationMinutes: durationMinutes,
				TotalMaxCount:   totalMaxCount,
				SuccessMaxCount: successMaxCount,
			})
		}

		// ------------------------------
		// 2) 基于 IP 的模型请求限流扩展（用户 / 分组 / 令牌）
		// ------------------------------
		if ipEnabled {
			clientIp := common.GetContextKeyString(c, constant.ContextKeyClientIP)
			if clientIp == "" {
				clientIp = c.ClientIP()
			}

			ipDurationMinutes := setting.ModelRequestIPRateLimitDurationMinutes

			// user + ip
			userId := common.GetContextKeyInt(c, constant.ContextKeyUserId)
			if userId == 0 {
				userId = c.GetInt("id")
			}
			if userId > 0 {
				policies = appendPolicyIfHasLimit(policies, modelRateLimitPolicy{
					Identifier:      fmt.Sprintf("ip:u:%d:%s", userId, clientIp),
					DurationMinutes: ipDurationMinutes,
					TotalMaxCount:   setting.ModelRequestIPRateLimitUserCount,
					SuccessMaxCount: setting.ModelRequestIPRateLimitUserSuccessCount,
				})
			}

			// group + ip
			if group != "" {
				policies = appendPolicyIfHasLimit(policies, modelRateLimitPolicy{
					Identifier:      fmt.Sprintf("ip:g:%s:%s", group, clientIp),
					DurationMinutes: ipDurationMinutes,
					TotalMaxCount:   setting.ModelRequestIPRateLimitGroupCount,
					SuccessMaxCount: setting.ModelRequestIPRateLimitGroupSuccessCount,
				})
			}

			// token + ip
			tokenId := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
			if tokenId > 0 {
				policies = appendPolicyIfHasLimit(policies, modelRateLimitPolicy{
					Identifier:      fmt.Sprintf("ip:t:%d:%s", tokenId, clientIp),
					DurationMinutes: ipDurationMinutes,
					TotalMaxCount:   setting.ModelRequestIPRateLimitTokenCount,
					SuccessMaxCount: setting.ModelRequestIPRateLimitTokenSuccessCount,
				})
			}
		}

		if len(policies) == 0 {
			c.Next()
			return
		}

		if common.RedisEnabled {
			enforceRedisModelRateLimit(c, policies)
		} else {
			enforceMemoryModelRateLimit(c, policies)
		}
	}
}
