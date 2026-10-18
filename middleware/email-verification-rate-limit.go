package middleware

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/limiter"

	"github.com/gin-gonic/gin"
)

const (
	EmailVerificationRateLimitMark = "EV"
	EmailVerificationMaxRequests   = 2  // 30秒内最多2次
	EmailVerificationDuration      = 30 // 30秒时间窗口
)

func redisEmailVerificationRateLimiter(c *gin.Context) {
	ctx, cancel := newRateLimitRedisContext()
	defer cancel()
	shard := common.HashShard(c.ClientIP(), common.RateLimitKeyShardCount)
	key := fmt.Sprintf("rateLimit:global:%s:ip:%s:%s", EmailVerificationRateLimitMark, c.ClientIP(), shard)
	lim := limiter.New(ctx, common.RDB)
	expireSeconds := int64(common.RateLimitKeyExpirationDuration.Seconds())
	allowed, err := lim.SlidingWindow(ctx, key, EmailVerificationMaxRequests, EmailVerificationDuration, expireSeconds, limiter.SlidingWindowModeCheckAndRecord)
	if err != nil {
		memoryEmailVerificationRateLimiter(c)
		return
	}
	if allowed {
		c.Next()
		return
	}

	common.ApiErrorStatus(c, http.StatusTooManyRequests, common.NewMessage("Please wait {{seconds}} seconds before requesting another verification code.", map[string]any{"seconds": int64(EmailVerificationDuration)}))
	c.Abort()
}

func memoryEmailVerificationRateLimiter(c *gin.Context) {
	key := EmailVerificationRateLimitMark + ":" + c.ClientIP()

	if !inMemoryRateLimiter.Request(key, EmailVerificationMaxRequests, EmailVerificationDuration) {
		common.ApiErrorStatus(c, http.StatusTooManyRequests, common.NewMessage("Please wait before requesting another verification code."))
		c.Abort()
		return
	}

	c.Next()
}

func EmailVerificationRateLimit() gin.HandlerFunc {
	// Keep the fallback ready before requests arrive so a concurrent Redis
	// outage cannot race the in-memory limiter's first initialization.
	inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
	return func(c *gin.Context) {
		if common.RedisEnabled {
			redisEmailVerificationRateLimiter(c)
		} else {
			memoryEmailVerificationRateLimiter(c)
		}
	}
}
