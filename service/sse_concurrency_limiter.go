package service

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

type sseConcurrencyCounter struct {
	count          atomic.Int64
	lastActiveUnix atomic.Int64
}

type sseConcurrencyTarget struct {
	entry *sseConcurrencyCounter
	key   string
	limit int
	scope string
}

const (
	sseConcurrencyCounterCleanupInterval = 256
	sseConcurrencyCounterIdleTTL         = 10 * time.Minute
)

var (
	sseConcurrencyCounters       sync.Map // map[string]*sseConcurrencyCounter
	sseConcurrencyCleanupCounter atomic.Uint64
	sseConcurrencyCountersMu     sync.Mutex
)

func getOrCreateSSEConcurrencyCounter(key string) *sseConcurrencyCounter {
	nowUnix := time.Now().Unix()
	if key == "" {
		counter := &sseConcurrencyCounter{}
		counter.lastActiveUnix.Store(nowUnix)
		return counter
	}
	if value, ok := sseConcurrencyCounters.Load(key); ok {
		if counter, ok := value.(*sseConcurrencyCounter); ok {
			counter.lastActiveUnix.Store(nowUnix)
			return counter
		}
		sseConcurrencyCounters.Delete(key)
	}
	counter := &sseConcurrencyCounter{}
	counter.lastActiveUnix.Store(nowUnix)
	actual, _ := sseConcurrencyCounters.LoadOrStore(key, counter)
	if actualCounter, ok := actual.(*sseConcurrencyCounter); ok {
		actualCounter.lastActiveUnix.Store(nowUnix)
		return actualCounter
	}
	return counter
}

func maybeCleanupSSEConcurrencyCounters() {
	if sseConcurrencyCleanupCounter.Add(1)%sseConcurrencyCounterCleanupInterval != 0 {
		return
	}
	sseConcurrencyCountersMu.Lock()
	defer sseConcurrencyCountersMu.Unlock()

	nowUnix := time.Now().Unix()
	sseConcurrencyCounters.Range(func(key, value any) bool {
		counter, ok := value.(*sseConcurrencyCounter)
		if !ok {
			sseConcurrencyCounters.Delete(key)
			return true
		}
		if counter.count.Load() != 0 {
			return true
		}
		if nowUnix-counter.lastActiveUnix.Load() < int64(sseConcurrencyCounterIdleTTL.Seconds()) {
			return true
		}
		sseConcurrencyCounters.CompareAndDelete(key, value)
		return true
	})
}

func decrementSSEConcurrencyCounter(_ string, counter *sseConcurrencyCounter) {
	if counter == nil {
		return
	}
	current := counter.count.Add(-1)
	if current < 0 {
		counter.count.Store(0)
	}
	counter.lastActiveUnix.Store(time.Now().Unix())
}

// AcquireSSEConcurrencySlot 为 SSE 请求申请并发槽位。
// 返回的 release 必须在请求结束时调用；若超过限制则返回错误。
func AcquireSSEConcurrencySlot(userID int, tokenID int) (release func(), err error) {
	setting := operation_setting.GetGeneralSetting()
	if setting == nil || !setting.SSEConcurrencyLimitEnabled {
		return func() {}, nil
	}
	maybeCleanupSSEConcurrencyCounters()

	sseConcurrencyCountersMu.Lock()
	defer sseConcurrencyCountersMu.Unlock()

	targets := make([]sseConcurrencyTarget, 0, 2)
	if setting.SSEMaxConcurrentPerUser > 0 && userID > 0 {
		key := fmt.Sprintf("sse:user:%d", userID)
		targets = append(targets, sseConcurrencyTarget{
			entry: getOrCreateSSEConcurrencyCounter(key),
			key:   key,
			limit: setting.SSEMaxConcurrentPerUser,
			scope: "user",
		})
	}
	if setting.SSEMaxConcurrentPerToken > 0 && tokenID > 0 {
		key := fmt.Sprintf("sse:token:%d", tokenID)
		targets = append(targets, sseConcurrencyTarget{
			entry: getOrCreateSSEConcurrencyCounter(key),
			key:   key,
			limit: setting.SSEMaxConcurrentPerToken,
			scope: "token",
		})
	}
	if len(targets) == 0 {
		return func() {}, nil
	}

	acquired := make([]sseConcurrencyTarget, 0, len(targets))
	for _, target := range targets {
		current := target.entry.count.Add(1)
		target.entry.lastActiveUnix.Store(time.Now().Unix())
		if current > int64(target.limit) {
			decrementSSEConcurrencyCounter(target.key, target.entry)
			for _, item := range acquired {
				decrementSSEConcurrencyCounter(item.key, item.entry)
			}
			return func() {}, fmt.Errorf("too many concurrent sse streams (%s limit exceeded)", target.scope)
		}
		acquired = append(acquired, target)
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, item := range acquired {
				decrementSSEConcurrencyCounter(item.key, item.entry)
			}
		})
	}
	return release, nil
}
