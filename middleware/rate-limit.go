package middleware

import (
	"context"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/common/limiter"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

var inMemoryRateLimiter common.InMemoryRateLimiter

var defNext = func(c *gin.Context) {
	c.Next()
}

func newRateLimitRedisContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), common.RateLimitRedisOpTimeout)
}

func redisRateLimiter(c *gin.Context, maxRequestNum int, duration int64, mark string) {
	ctx, cancel := newRateLimitRedisContext()
	defer cancel()
	rdb := common.RDB
	shard := common.HashShard(c.ClientIP(), common.RateLimitKeyShardCount)
	key := fmt.Sprintf("rateLimit:global:%s:ip:%s:%s", mark, c.ClientIP(), shard)
	lim := limiter.New(ctx, rdb)
	expireSeconds := int64(common.RateLimitKeyExpirationDuration.Seconds())
	allowed, err := lim.SlidingWindow(ctx, key, maxRequestNum, duration, expireSeconds, limiter.SlidingWindowModeCheckAndRecord)
	if err != nil {
		fmt.Println(err.Error())
		c.Status(http.StatusInternalServerError)
		c.Abort()
		return
	}
	if !allowed {
		c.Status(http.StatusTooManyRequests)
		c.Abort()
		return
	}
}

func memoryRateLimiter(c *gin.Context, maxRequestNum int, duration int64, mark string) {
	key := fmt.Sprintf("global:%s:ip:%s", mark, c.ClientIP())
	if !inMemoryRateLimiter.Request(key, maxRequestNum, duration) {
		c.Status(http.StatusTooManyRequests)
		c.Abort()
		return
	}
}

func rateLimitFactory(maxRequestNum int, duration int64, mark string) func(c *gin.Context) {
	if common.RedisEnabled {
		return func(c *gin.Context) {
			redisRateLimiter(c, maxRequestNum, duration, mark)
		}
	} else {
		// It's safe to call multi times.
		inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
		return func(c *gin.Context) {
			memoryRateLimiter(c, maxRequestNum, duration, mark)
		}
	}
}

// GlobalWebRateLimit limits dashboard pages and other fallback paths per IP.
// Files that exist in the frontend build count against a separate static
// limiter instead, so lazy-loaded chunks and icons do not use up the page
// budget. The static limiter is disabled by default.
func GlobalWebRateLimit(frontendFS static.ServeFileSystem) func(c *gin.Context) {
	webLimit := defNext
	if common.GlobalWebRateLimitEnable {
		webLimit = rateLimitFactory(common.GlobalWebRateLimitNum, common.GlobalWebRateLimitDuration, "GW")
	}
	staticLimit := defNext
	if common.GlobalStaticRateLimitEnable {
		staticLimit = rateLimitFactory(common.GlobalStaticRateLimitNum, common.GlobalStaticRateLimitDuration, "GS")
	}
	return func(c *gin.Context) {
		if frontendFS.Exists("/", c.Request.URL.Path) {
			staticLimit(c)
			return
		}
		webLimit(c)
	}
}

func GlobalAPIRateLimit() func(c *gin.Context) {
	if common.GlobalApiRateLimitEnable {
		return rateLimitFactory(common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration, "GA")
	}
	return defNext
}

func CriticalRateLimit() func(c *gin.Context) {
	if common.CriticalRateLimitEnable {
		return rateLimitFactory(common.CriticalRateLimitNum, common.CriticalRateLimitDuration, "CT")
	}
	return defNext
}

func UserCriticalRateLimit(scope string) func(c *gin.Context) {
	if !common.CriticalRateLimitEnable {
		return defNext
	}
	return userRateLimitFactory(
		common.CriticalRateLimitNum,
		common.CriticalRateLimitDuration,
		"UC:"+scope,
	)
}

func DownloadRateLimit() func(c *gin.Context) {
	return rateLimitFactory(common.DownloadRateLimitNum, common.DownloadRateLimitDuration, "DW")
}

func UploadRateLimit() func(c *gin.Context) {
	return rateLimitFactory(common.UploadRateLimitNum, common.UploadRateLimitDuration, "UP")
}

// userRateLimitFactory creates a rate limiter keyed by authenticated user ID
// instead of client IP, making it resistant to proxy rotation attacks.
// Must be used AFTER authentication middleware (UserAuth).
func userRateLimitFactory(maxRequestNum int, duration int64, mark string) func(c *gin.Context) {
	if common.RedisEnabled {
		return func(c *gin.Context) {
			userId := c.GetInt("id")
			if userId == 0 {
				c.Status(http.StatusUnauthorized)
				c.Abort()
				return
			}
			shard := common.HashShard(fmt.Sprintf("%d", userId), common.RateLimitKeyShardCount)
			key := fmt.Sprintf("rateLimit:user:%s:id:%d:%s", mark, userId, shard)
			userRedisRateLimiter(c, maxRequestNum, duration, key)
		}
	}
	// It's safe to call multi times.
	inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
	return func(c *gin.Context) {
		userId := c.GetInt("id")
		if userId == 0 {
			c.Status(http.StatusUnauthorized)
			c.Abort()
			return
		}
		key := fmt.Sprintf("user:%s:id:%d", mark, userId)
		if !inMemoryRateLimiter.Request(key, maxRequestNum, duration) {
			c.Status(http.StatusTooManyRequests)
			c.Abort()
			return
		}
	}
}

// userRedisRateLimiter is like redisRateLimiter but accepts a pre-built key
// (to support user-ID-based keys).
func userRedisRateLimiter(c *gin.Context, maxRequestNum int, duration int64, key string) {
	ctx, cancel := newRateLimitRedisContext()
	defer cancel()
	rdb := common.RDB
	lim := limiter.New(ctx, rdb)
	expireSeconds := int64(common.RateLimitKeyExpirationDuration.Seconds())
	allowed, err := lim.SlidingWindow(ctx, key, maxRequestNum, duration, expireSeconds, limiter.SlidingWindowModeCheckAndRecord)
	if err != nil {
		fmt.Println(err.Error())
		c.Status(http.StatusInternalServerError)
		c.Abort()
		return
	}
	if !allowed {
		c.Status(http.StatusTooManyRequests)
		c.Abort()
		return
	}
}

// SearchRateLimit returns a per-user rate limiter for search endpoints.
// 10 requests per 60 seconds per user (by user ID, not IP).
func SearchRateLimit() func(c *gin.Context) {
	return userRateLimitFactory(common.SearchRateLimitNum, common.SearchRateLimitDuration, "SR")
}
