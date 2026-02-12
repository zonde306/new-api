package limiter

import (
	"context"
	_ "embed"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
)

//go:embed lua/rate_limit.lua
var rateLimitScript string

//go:embed lua/sliding_window.lua
var slidingWindowScript string

const (
	SlidingWindowModeCheck         = 0
	SlidingWindowModeCheckAndRecord = 1
	SlidingWindowModeRecord        = 2
)

type RedisLimiter struct {
	client                 *redis.Client
	limitScriptSHA         string
	slidingWindowScriptSHA string
}

var (
	instance *RedisLimiter
	once     sync.Once
)

func New(ctx context.Context, r *redis.Client) *RedisLimiter {
	once.Do(func() {
		// 预加载脚本
		limitSHA, err := r.ScriptLoad(ctx, rateLimitScript).Result()
		if err != nil {
			common.SysLog(fmt.Sprintf("Failed to load rate limit script: %v", err))
		}
		slidingWindowSHA, err := r.ScriptLoad(ctx, slidingWindowScript).Result()
		if err != nil {
			common.SysLog(fmt.Sprintf("Failed to load sliding window script: %v", err))
		}
		instance = &RedisLimiter{
			client:                 r,
			limitScriptSHA:         limitSHA,
			slidingWindowScriptSHA: slidingWindowSHA,
		}
	})

	return instance
}

func (rl *RedisLimiter) Allow(ctx context.Context, key string, opts ...Option) (bool, error) {
	// 默认配置
	config := &Config{
		Capacity:  10,
		Rate:      1,
		Requested: 1,
	}

	// 应用选项模式
	for _, opt := range opts {
		opt(config)
	}

	// 执行限流
	result, err := rl.client.EvalSha(
		ctx,
		rl.limitScriptSHA,
		[]string{key},
		config.Requested,
		config.Rate,
		config.Capacity,
	).Int()

	if err != nil {
		return false, fmt.Errorf("rate limit failed: %w", err)
	}
	return result == 1, nil
}

func (rl *RedisLimiter) SlidingWindow(ctx context.Context, key string, maxRequestNum int, windowSeconds int64, expireSeconds int64, mode int) (bool, error) {
	if maxRequestNum <= 0 {
		return true, nil
	}
	if windowSeconds <= 0 {
		return true, nil
	}
	result, err := rl.client.EvalSha(
		ctx,
		rl.slidingWindowScriptSHA,
		[]string{key},
		maxRequestNum,
		windowSeconds,
		expireSeconds,
		mode,
	).Int()
	if err != nil {
		return false, fmt.Errorf("sliding window rate limit failed: %w", err)
	}
	return result == 1, nil
}

// Config 配置选项模式
type Config struct {
	Capacity  int64
	Rate      int64
	Requested int64
}

type Option func(*Config)

func WithCapacity(c int64) Option {
	return func(cfg *Config) { cfg.Capacity = c }
}

func WithRate(r int64) Option {
	return func(cfg *Config) { cfg.Rate = r }
}

func WithRequested(n int64) Option {
	return func(cfg *Config) { cfg.Requested = n }
}
