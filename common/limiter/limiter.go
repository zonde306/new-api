package limiter

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
)

//go:embed lua/rate_limit.lua
var rateLimitScript string

//go:embed lua/sliding_window.lua
var slidingWindowScript string

const (
	SlidingWindowModeCheck          = 0
	SlidingWindowModeCheckAndRecord = 1
	SlidingWindowModeRecord         = 2
	SlidingWindowModeRollback       = 3
)

type RedisLimiter struct {
	client                 *redis.Client
	limitScriptSHA         string
	slidingWindowScriptSHA string
	mu                     sync.RWMutex
}

var (
	instance *RedisLimiter
	once     sync.Once
)

func New(ctx context.Context, r *redis.Client) *RedisLimiter {
	once.Do(func() {
		instance = &RedisLimiter{client: r}
	})
	if instance != nil && instance.client == nil {
		instance.client = r
	}
	// 避免每次请求都 SCRIPT LOAD，仅在首次/丢失 SHA 时加载。
	if instance.getRateScriptSHA() == "" || instance.getSlidingWindowScriptSHA() == "" {
		if err := instance.loadScripts(ctx); err != nil {
			common.SysLog(fmt.Sprintf("Failed to preload limiter scripts: %v", err))
		}
	}
	return instance
}

func (rl *RedisLimiter) loadScripts(ctx context.Context) error {
	var errs []string
	if err := rl.loadRateScript(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("rate_limit.lua: %v", err))
	}
	if err := rl.loadSlidingWindowScript(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("sliding_window.lua: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (rl *RedisLimiter) loadRateScript(ctx context.Context) error {
	sha, err := rl.client.ScriptLoad(ctx, rateLimitScript).Result()
	if err != nil {
		return err
	}
	rl.mu.Lock()
	rl.limitScriptSHA = sha
	rl.mu.Unlock()
	return nil
}

func (rl *RedisLimiter) loadSlidingWindowScript(ctx context.Context) error {
	sha, err := rl.client.ScriptLoad(ctx, slidingWindowScript).Result()
	if err != nil {
		return err
	}
	rl.mu.Lock()
	rl.slidingWindowScriptSHA = sha
	rl.mu.Unlock()
	return nil
}

func (rl *RedisLimiter) getRateScriptSHA() string {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.limitScriptSHA
}

func (rl *RedisLimiter) getSlidingWindowScriptSHA() string {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.slidingWindowScriptSHA
}

func isNoScriptErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToUpper(err.Error()), "NOSCRIPT")
}

func (rl *RedisLimiter) evalRateLimit(ctx context.Context, key string, args ...interface{}) (int, error) {
	sha := rl.getRateScriptSHA()
	if sha != "" {
		res, err := rl.client.EvalSha(ctx, sha, []string{key}, args...).Int()
		if err == nil {
			return res, nil
		}
		if !isNoScriptErr(err) {
			return 0, err
		}
	}

	if err := rl.loadRateScript(ctx); err == nil {
		sha = rl.getRateScriptSHA()
		if sha != "" {
			res, err := rl.client.EvalSha(ctx, sha, []string{key}, args...).Int()
			if err == nil {
				return res, nil
			}
			if !isNoScriptErr(err) {
				return 0, err
			}
		}
	}

	return rl.client.Eval(ctx, rateLimitScript, []string{key}, args...).Int()
}

func (rl *RedisLimiter) evalSlidingWindow(ctx context.Context, key string, args ...interface{}) (int, error) {
	sha := rl.getSlidingWindowScriptSHA()
	if sha != "" {
		res, err := rl.client.EvalSha(ctx, sha, []string{key}, args...).Int()
		if err == nil {
			return res, nil
		}
		if !isNoScriptErr(err) {
			return 0, err
		}
	}

	if err := rl.loadSlidingWindowScript(ctx); err == nil {
		sha = rl.getSlidingWindowScriptSHA()
		if sha != "" {
			res, err := rl.client.EvalSha(ctx, sha, []string{key}, args...).Int()
			if err == nil {
				return res, nil
			}
			if !isNoScriptErr(err) {
				return 0, err
			}
		}
	}

	return rl.client.Eval(ctx, slidingWindowScript, []string{key}, args...).Int()
}

func (rl *RedisLimiter) Allow(ctx context.Context, key string, opts ...Option) (bool, error) {
	// 默认配置
	config := &Config{
		Capacity:      10,
		Rate:          1,
		Requested:     1,
		ExpireSeconds: 0,
	}

	// 应用选项模式
	for _, opt := range opts {
		opt(config)
	}

	// 执行限流
	result, err := rl.evalRateLimit(ctx, key, config.Requested, config.Rate, config.Capacity, config.ExpireSeconds)
	if err != nil {
		return false, fmt.Errorf("rate limit failed: %w", err)
	}
	return result == 1, nil
}

func (rl *RedisLimiter) SlidingWindow(ctx context.Context, key string, maxRequestNum int, windowSeconds int64, expireSeconds int64, mode int) (bool, error) {
	return rl.SlidingWindowWithEntry(ctx, key, maxRequestNum, windowSeconds, expireSeconds, mode, "")
}

func (rl *RedisLimiter) SlidingWindowWithEntry(ctx context.Context, key string, maxRequestNum int, windowSeconds int64, expireSeconds int64, mode int, entry string) (bool, error) {
	if mode != SlidingWindowModeRollback {
		if maxRequestNum <= 0 {
			return true, nil
		}
		if windowSeconds <= 0 {
			return true, nil
		}
	}
	result, err := rl.evalSlidingWindow(ctx, key, maxRequestNum, windowSeconds, expireSeconds, mode, entry)
	if err != nil {
		return false, fmt.Errorf("sliding window rate limit failed: %w", err)
	}
	return result == 1, nil
}

// Config 配置选项模式
type Config struct {
	Capacity      int64
	Rate          int64
	Requested     int64
	ExpireSeconds int64
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

func WithExpireSeconds(seconds int64) Option {
	return func(cfg *Config) { cfg.ExpireSeconds = seconds }
}
