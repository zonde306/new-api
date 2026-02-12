# P1 DB 写入与批量更新优化计划

## 目标
- 降低高并发写入对数据库的锁竞争与事务等待。
- 缓解后台批量落库积压与长尾延迟。
- 在 SQLite MySQL PostgreSQL 多库兼容前提下优化写入路径。

## 现状观察与定位
- 数据看板缓存持锁期间执行多次 DB 查询与更新，易形成串行瓶颈：
  - [SaveQuotaDataCache()](model/usedata.go:67)
  - [increaseQuotaData()](model/usedata.go:92)
- 批量更新入队频繁加锁，单协程周期落库，DB 慢时积压：
  - [addNewRecord()](model/utils.go:42)
  - [InitBatchUpdater()](model/utils.go:33)
  - [batchUpdate()](model/utils.go:52)

## 优化策略（按实施优先级）

### 1. 数据看板缓存写入“锁外落库”
- 方案
  - 先复制缓存 map 到局部变量，再释放锁，后续 DB 写入在锁外执行。
  - 写入完成后再清空共享 map，避免长时间持锁。
- 涉及位置
  - [SaveQuotaDataCache()](model/usedata.go:67)
- 成功标准
  - 高并发记录时锁等待显著下降。
  - DB 写入抖动不会阻塞日志记录。

### 2. 批量更新从单协程改为分片并行
- 方案
  - 按类型或 key hash 分片并行落库，减少单线程瓶颈。
  - 配置并发度上限，避免过度并发压垮 DB。
- 涉及位置
  - [batchUpdate()](model/utils.go:52)
- 成功标准
  - 批量更新积压长度下降。
  - 落库延迟显著缩短。

### 3. 将高频更新改为 DB 原子更新批次
- 方案
  - 合并多次 update 为单次 update 语句，减少 DB 往返。
  - 结合 GORM 的 Updates 与 Expr 批量更新。
- 涉及位置
  - [increaseQuotaData()](model/usedata.go:92)
  - [updateUserUsedQuotaAndRequestCount()](model/user.go:913)
- 成功标准
  - DB QPS 降低，吞吐提升。

### 4. 按库配置连接池与事务策略
- 方案
  - 配置 GORM 连接池参数，避免连接耗尽。
  - 对长事务进行拆分或简化。
- 涉及位置
  - DB 初始化相关模块
- 成功标准
  - DB 连接池等待时间下降。
  - 长事务锁等待减少。

## 实施步骤
1. 重构数据看板缓存写入为锁外落库。
2. 批量更新逻辑引入分片并行与配置化并发度。
3. 高频更新聚合与减少 DB 往返。
4. 加入连接池与事务调优参数，验证多库兼容。

## 风险与回滚
- 风险
  - 并行落库可能导致 DB 短时间负载峰值。
  - 锁外落库需要保证数据一致性。
- 回滚策略
  - 保留串行模式与开关。

## 依赖与验收
- 依赖
  - DB 慢查询与锁等待监控。
  - 压测对比基线。
- 验收
  - 批量更新落库延迟下降。
  - 高并发下 DB 连接池耗尽告警减少。

## 并发链路示意

```mermaid
flowchart LR
  A[请求写入指标] --> B[缓存入队]
  B --> C[批量聚合]
  C --> D[DB 写入]
```
