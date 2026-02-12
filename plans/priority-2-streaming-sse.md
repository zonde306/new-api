# P2 流式 SSE 与长连接资源优化计划

## 目标
- 降低长连接对 goroutine 定时器与写锁的占用。
- 避免流式请求在高并发下拖垮 CPU 与内存。
- 保持 SSE 语义与稳定性。

## 现状观察与定位
- 每个流式请求会创建多个 goroutine 与 ticker，写入路径通过互斥锁串行化：
  - [StreamScannerHandler()](relay/helper/stream_scanner.go:37)
  - [writeMutex](relay/helper/stream_scanner.go:57)
- 对 ping 与 data handler 的超时保护使用 goroutine 包裹写锁，频繁分配：
  - [StreamScannerHandler()](relay/helper/stream_scanner.go:116)
  - [StreamScannerHandler()](relay/helper/stream_scanner.go:223)

## 优化策略（按实施优先级）

### 1. 降低 goroutine 与定时器创建频率
- 方案
  - 将 ping ticker 与 data handler 超时机制合并为单一调度器，减少 goroutine 数量。
  - 复用定时器或采用 time.AfterFunc 池化策略。
- 涉及位置
  - [StreamScannerHandler()](relay/helper/stream_scanner.go:37)
- 成功标准
  - 单连接 goroutine 数下降。
  - 连接数增加时 CPU 消耗线性降低。

### 2. 降低写锁持有与等待
- 方案
  - 采用有界 channel 序列化写入，替代共享 mutex。
  - 写入失败时尽快退出，避免锁竞争扩散。
- 涉及位置
  - [writeMutex](relay/helper/stream_scanner.go:57)
- 成功标准
  - 写入锁等待时间下降。
  - SSE 输出更平稳。

### 3. SSE 连接限流与最大并发限制
- 方案
  - 引入 per-user 或 per-token SSE 并发上限。
  - 提供管理配置与动态调节。
- 涉及位置
  - SSE router 或 middleware 层
- 成功标准
  - 单用户高并发不再压垮系统。
  - 长连接资源可控。

### 4. 连接生命周期优化与回收
- 方案
  - 对 ping goroutine 设置更严格的回收机制。
  - 客户端断连后尽快清理。
- 涉及位置
  - [StreamScannerHandler()](relay/helper/stream_scanner.go:82)
- 成功标准
  - 泄漏连接数量减少。
  - 长期运行稳定。

## 实施步骤
1. 评估单连接 goroutine 与 ticker 数量，建立基线。
2. 重构写入管线，减少 goroutine 与锁竞争。
3. 增加 SSE 并发限制与配置化管理。
4. 压测与稳定性回归。

## 风险与回滚
- 风险
  - 写入序列化变更可能影响输出顺序。
  - 并发限制可能影响部分用户体验。
- 回滚策略
  - 保留现有实现为开关。

## 依赖与验收
- 依赖
  - 连接数与 goroutine 指标采集。
  - SSE 输出顺序与完整性测试。
- 验收
  - 单连接资源占用下降。
  - SSE 连接数上升时系统稳定。

## 并发链路示意

```mermaid
flowchart LR
  A[客户端连接] --> B[流式处理]
  B --> C[写入调度]
  C --> D[响应输出]
```
