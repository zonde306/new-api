package model

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

const (
	BatchUpdateTypeUserQuota = iota
	BatchUpdateTypeTokenQuota
	BatchUpdateTypeUsedQuota
	BatchUpdateTypeChannelUsedQuota
	BatchUpdateTypeRequestCount
	BatchUpdateTypeCount // if you add a new type, you need to add a new map and a new lock
)

var batchUpdateStores []map[int]int
var batchUpdateLocks []sync.Mutex

func init() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateStores = append(batchUpdateStores, make(map[int]int))
		batchUpdateLocks = append(batchUpdateLocks, sync.Mutex{})
	}
}

func InitBatchUpdater() {
	gopool.Go(func() {
		for {
			time.Sleep(time.Duration(common.BatchUpdateInterval) * time.Second)
			batchUpdate()
		}
	})
}

func addNewRecord(type_ int, id int, value int) {
	batchUpdateLocks[type_].Lock()
	defer batchUpdateLocks[type_].Unlock()
	if _, ok := batchUpdateStores[type_][id]; !ok {
		batchUpdateStores[type_][id] = value
	} else {
		batchUpdateStores[type_][id] += value
	}
}

type batchUpdateRecord struct {
	key   int
	value int
}

func getBatchUpdateWorkerCount(total int) int {
	if total <= 1 {
		return total
	}
	if common.UsingSQLite {
		return 1
	}
	workerCount := common.BatchUpdateConcurrency
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > common.BatchUpdateConcurrencyMax {
		workerCount = common.BatchUpdateConcurrencyMax
	}
	if workerCount > total {
		workerCount = total
	}
	return workerCount
}

func batchShardIndex(key int, workerCount int) int {
	if workerCount <= 1 {
		return 0
	}
	hash := uint64(uint(key))
	hash ^= hash >> 33
	hash *= 0xff51afd7ed558ccd
	hash ^= hash >> 33
	return int(hash % uint64(workerCount))
}

const batchUpdateRetryMaxAttempts = 3

func isRetryableBatchUpdateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "could not serialize access")
}

func applyBatchUpdate(type_ int, key int, value int) error {
	switch type_ {
	case BatchUpdateTypeUserQuota:
		return increaseUserQuota(key, value)
	case BatchUpdateTypeTokenQuota:
		return increaseTokenQuota(key, value)
	case BatchUpdateTypeUsedQuota:
		return updateUserUsedQuota(key, value)
	case BatchUpdateTypeRequestCount:
		return updateUserRequestCount(key, value)
	case BatchUpdateTypeChannelUsedQuota:
		return updateChannelUsedQuota(key, value)
	default:
		return fmt.Errorf("unsupported batch update type: %d", type_)
	}
}

func processSingleBatchRecord(type_ int, key int, value int) {
	var err error
	for attempt := 1; attempt <= batchUpdateRetryMaxAttempts; attempt++ {
		err = applyBatchUpdate(type_, key, value)
		if err == nil {
			return
		}
		if !isRetryableBatchUpdateError(err) || attempt == batchUpdateRetryMaxAttempts {
			break
		}
		time.Sleep(time.Duration(attempt*50) * time.Millisecond)
	}

	common.SysLog(fmt.Sprintf("failed to batch update(type=%d,key=%d,value=%d), re-queued: %v", type_, key, value, err))
	addNewRecord(type_, key, value)
}

func processBatchStore(type_ int, store map[int]int) {
	if len(store) == 0 {
		return
	}

	workerCount := getBatchUpdateWorkerCount(len(store))
	if workerCount <= 1 {
		for key, value := range store {
			processSingleBatchRecord(type_, key, value)
		}
		return
	}

	shards := make([][]batchUpdateRecord, workerCount)
	for key, value := range store {
		idx := batchShardIndex(key, workerCount)
		shards[idx] = append(shards[idx], batchUpdateRecord{key: key, value: value})
	}

	var wg sync.WaitGroup
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		records := shards[i]
		go func(records []batchUpdateRecord) {
			defer wg.Done()
			for _, record := range records {
				processSingleBatchRecord(type_, record.key, record.value)
			}
		}(records)
	}
	wg.Wait()
}

func batchUpdate() {
	// check if there's any data to update
	hasData := false
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		if len(batchUpdateStores[i]) > 0 {
			hasData = true
			batchUpdateLocks[i].Unlock()
			break
		}
		batchUpdateLocks[i].Unlock()
	}

	if !hasData {
		return
	}

	common.SysLog("batch update started")
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		store := batchUpdateStores[i]
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
		processBatchStore(i, store)
	}
	common.SysLog("batch update finished")
}

func RecordExist(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

func shouldUpdateRedis(fromDB bool, err error) bool {
	return common.RedisEnabled && fromDB && err == nil
}
