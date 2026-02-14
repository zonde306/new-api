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

// Redis限流处理器
func redisRateLimitHandler(duration int64, durationMinutes int, identifier string, totalMaxCount, successMaxCount int) gin.HandlerFunc {
	return func(c *gin.Context) {
		rdb := common.RDB

		// 1. 成功请求限制采用“检查并预记录”，失败后回滚，减少一次 Redis 往返
		shard := common.HashShard(identifier, common.RateLimitKeyShardCount)
		successKey := fmt.Sprintf("rateLimit:model:%s:id:%s:%s", ModelRequestRateLimitSuccessCountMark, identifier, shard)
		requestEntrySuffix := ""
		allowed := true
		var err error

		if successMaxCount > 0 {
			// 仅传递唯一后缀，时间前缀由 Redis Lua 使用 TIME 生成，避免应用机与 Redis 时钟偏差
			requestEntrySuffix = common.GetUUID()
			allowed, err = checkAndRecordSuccessRequest(rdb, successKey, successMaxCount, duration, durationMinutes, requestEntrySuffix)
			if err != nil {
				fmt.Println("检查成功请求数限制失败:", err.Error())
				abortWithOpenAiMessage(c, http.StatusInternalServerError, "rate_limit_check_failed")
				return
			}
			if !allowed {
				abortWithOpenAiMessage(c, http.StatusTooManyRequests, fmt.Sprintf("您已达到请求数限制：%d分钟内最多请求%d次", durationMinutes, successMaxCount))
				return
			}
		}

		// 2. 检查总请求数限制（包含失败请求）
		if totalMaxCount > 0 {
			totalKey := fmt.Sprintf("rateLimit:model:%s:id:%s:%s", ModelRequestRateLimitCountMark, identifier, shard)
			ctx, cancel := newModelRateLimitRedisContext()
			tb := limiter.New(ctx, rdb)
			allowed, err = tb.Allow(
				ctx,
				totalKey,
				limiter.WithCapacity(int64(totalMaxCount)*duration),
				limiter.WithRate(int64(totalMaxCount)),
				limiter.WithRequested(duration),
				limiter.WithExpireSeconds(duration+60),
			)
			cancel()

			if err != nil {
				if requestEntrySuffix != "" {
					rollbackSuccessRequestWithRetry(rdb, successKey, durationMinutes, requestEntrySuffix)
				}
				fmt.Println("检查总请求数限制失败:", err.Error())
				abortWithOpenAiMessage(c, http.StatusInternalServerError, "rate_limit_check_failed")
				return
			}

			if !allowed {
				if requestEntrySuffix != "" {
					rollbackSuccessRequestWithRetry(rdb, successKey, durationMinutes, requestEntrySuffix)
				}
				abortWithOpenAiMessage(c, http.StatusTooManyRequests, fmt.Sprintf("您已达到总请求数限制：%d分钟内最多请求%d次，包括失败次数，请检查您的请求是否正确", durationMinutes, totalMaxCount))
				return
			}
		}

		// 3. 处理请求
		c.Next()

		// 4. 请求失败则回滚“成功请求预记录”
		if requestEntrySuffix != "" && c.Writer.Status() >= 400 {
			rollbackSuccessRequestWithRetry(rdb, successKey, durationMinutes, requestEntrySuffix)
		}
	}
}

// 内存限流处理器
func memoryRateLimitHandler(duration int64, durationMinutes int, identifier string, totalMaxCount, successMaxCount int) gin.HandlerFunc {
	inMemoryRateLimiter.Init(time.Duration(durationMinutes) * time.Minute)

	return func(c *gin.Context) {
		totalKey := ModelRequestRateLimitCountMark + identifier
		successKey := ModelRequestRateLimitSuccessCountMark + identifier

		// 1. 合并判定（成功限制优先检查，不记录）
		if !inMemoryRateLimiter.AllowWithCheck(totalKey, totalMaxCount, successKey, successMaxCount, duration) {
			c.Status(http.StatusTooManyRequests)
			c.Abort()
			return
		}

		// 2. 处理请求
		c.Next()

		// 3. 如果请求成功，记录到实际的成功请求计数中
		if c.Writer.Status() < 400 {
			inMemoryRateLimiter.Request(successKey, successMaxCount, duration)
		}
	}
}

// ModelRequestRateLimit 模型请求限流中间件
func ModelRequestRateLimit() func(c *gin.Context) {
	return func(c *gin.Context) {
		// 在每个请求时检查是否启用限流
		systemEnabled := setting.ModelRequestRateLimitEnabled
		tokenRateLimitEnabled := common.GetContextKeyBool(c, constant.ContextKeyTokenRateLimitEnabled)
		if !systemEnabled && !tokenRateLimitEnabled {
			c.Next()
			return
		}

		// 计算系统限流参数
		systemDurationMinutes := 0
		systemTotalMaxCount := 0
		systemSuccessMaxCount := 0
		if systemEnabled {
			systemDurationMinutes = setting.ModelRequestRateLimitDurationMinutes
			systemTotalMaxCount = setting.ModelRequestRateLimitCount
			systemSuccessMaxCount = setting.ModelRequestRateLimitSuccessCount
		}

		// 获取分组
		group := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUserGroup)
		}

		// 获取分组的系统限流配置
		if systemEnabled {
			systemGroupTotalCount, systemGroupSuccessCount, found := setting.GetGroupRateLimit(group)
			if found {
				systemTotalMaxCount = systemGroupTotalCount
				systemSuccessMaxCount = systemGroupSuccessCount
			}
		}

		// 计算令牌限流参数
		tokenRateLimitEnabled = common.GetContextKeyBool(c, constant.ContextKeyTokenRateLimitEnabled)
		tokenDurationMinutes := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitDurationMins)
		tokenTotalMaxCount := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitCount)
		tokenSuccessMaxCount := common.GetContextKeyInt(c, constant.ContextKeyTokenRateLimitSuccessCount)

		// 汇总最终限流（系统优先，取更严格限制）
		durationMinutes := systemDurationMinutes
		totalMaxCount := systemTotalMaxCount
		successMaxCount := systemSuccessMaxCount
		hasLimit := totalMaxCount > 0 || successMaxCount > 0

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
			hasLimit = hasLimit || tokenTotalMaxCount > 0 || tokenSuccessMaxCount > 0
		}

		if !hasLimit {
			c.Next()
			return
		}

		duration := int64(durationMinutes * 60)

		// 根据存储类型选择并执行限流处理器
		identifier := strconv.Itoa(common.GetContextKeyInt(c, constant.ContextKeyTokenId))
		if identifier == "0" {
			identifier = strconv.Itoa(c.GetInt("id"))
		}
		if common.RedisEnabled {
			redisRateLimitHandler(duration, durationMinutes, identifier, totalMaxCount, successMaxCount)(c)
		} else {
			memoryRateLimitHandler(duration, durationMinutes, identifier, totalMaxCount, successMaxCount)(c)
		}
	}
}
