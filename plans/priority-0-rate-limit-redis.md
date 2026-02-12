# P0 限流与 Redis 优化计划

## 目标
- 降低限流链路对并发吞吐的影响，减少 Redis RTT 与热点 key 竞争。
- 降低 Redis 连接池等待与 Lua 脚本加载路径的尾延迟。
- 在不改变业务语义的前提下提升高并发稳定性。

## 现状观察与定位
- 全局/用户限流在每次请求中进行多次 Redis 往返，且使用 list 结构频繁 LLen/LPush/LIndex/Expire：
  - [redisRateLimiter()](middleware/rate-limit.go:21)
  - [userRedisRateLimiter()](middleware/rate-limit.go:153)
- 模型请求限流同时维护成功计数和总计数，Redis 操作更密集，且 token-bucket Lua 执行会放大高并发负载：
  - [redisRateLimitHandler()](middleware/model-rate-limit.go:78)
  - [checkRedisRateLimit()](middleware/model-rate-limit.go:25)
  - [recordRedisRequest()](middleware/model-rate-limit.go:65)
  - [RedisLimiter.Allow()](common/limiter/limiter.go:42)
- Redis 连接池默认 10，可能成为高并发瓶颈：
  - [InitRedisClient()](common/redis.go:24)

## 优化策略（按实施优先级）

### 1. 将 List 滑动窗口限流迁移为 Lua 脚本原子计数
- 方案
  - 用 Lua 脚本实现固定窗口或滑动窗口计数，单次 Eval 即完成读取与更新。
  - 统一 global/user/model 限流为同一套 Redis 脚本，降低 RTT。
- 涉及位置
  - [redisRateLimiter()](middleware/rate-limit.go:21)
  - [userRedisRateLimiter()](middleware/rate-limit.go:153)
  - [checkRedisRateLimit()](middleware/model-rate-limit.go:25)
  - [recordRedisRequest()](middleware/model-rate-limit.go:65)
- 成功标准
  - 每次限流 Redis 请求减少到 1 次。
  - 压测下 Redis CPU 与网络负载下降，P99 延迟下降。

### 2. 限流 key 设计优化与热点拆分
- 方案
  - 对单 key 高 QPS 场景引入 hash tag 或多 key 分片聚合，降低单 key 热点。
  - 分离 global 限流与 per-user 限流 key namespace，避免冲突。
- 涉及位置
  - [rateLimitFactory()](middleware/rate-limit.go:76)
  - [userRateLimitFactory()](middleware/rate-limit.go:122)
- 成功标准
  - Redis 热点 key 查询分布更均匀。
  - 单 key 队列长度或访问频次下降。

### 3. 连接池与超时配置
- 方案
  - 提供可配置 Redis PoolSize 与 Timeout 参数，并在生产环境做容量评估。
  - 结合实际 QPS 与 RTT 调整 PoolSize 与 MaxIdleConns。
- 涉及位置
  - [InitRedisClient()](common/redis.go:24)
- 成功标准
  - Redis pool wait time 显著下降。
  - 连接池耗尽告警减少。

### 4. 模型限流与全局限流的合并判定
- 方案
  - 将系统限流与 token 限流做更严格的合并判定，减少重复判断与 Redis 操作。
  - 在内存限流模式下减少多次 Request 调用。
- 涉及位置
  - [ModelRequestRateLimit()](middleware/model-rate-limit.go:165)
  - [memoryRateLimitHandler()](middleware/model-rate-limit.go:131)
- 成功标准
  - 内存限流路径锁竞争与 CPU 消耗下降。

## 实施步骤
1. 设计 Lua 脚本与统一限流接口，替换 list 方案。
2. 更新限流 key 策略，完成分片与 namespace 规范化。
3. 增加 Redis 连接池与超时配置，输出运行时指标。
4. 压测验证与回归测试，确认错误率与吞吐收益。

## 风险与回滚
- 风险
  - Lua 脚本错误导致限流不生效或过度限制。
  - key 分片策略改变可能影响统计语义。
- 回滚策略
  - 保留旧限流实现与开关，支持一键切回。

## 依赖与验收
- 依赖
  - Redis 可用性监控与慢查询统计。
  - 压测基线与指标采集。
- 验收
  - 限流路径平均 Redis RTT 下降。
  - 高并发下 P95 P99 延迟与错误率改善。

## 并发链路示意

```mermaid
flowchart LR
  A[请求进入] --> B[限流判断]
  B --> C[Redis 脚本执行]
  C --> D[放行或拒绝]
```
