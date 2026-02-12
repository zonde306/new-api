package common

import (
	"sync"
	"time"
)

type InMemoryRateLimiter struct {
	store              map[string]*[]int64
	mutex              sync.Mutex
	expirationDuration time.Duration
}

func (l *InMemoryRateLimiter) Init(expirationDuration time.Duration) {
	if l.store == nil {
		l.mutex.Lock()
		if l.store == nil {
			l.store = make(map[string]*[]int64)
			l.expirationDuration = expirationDuration
			if expirationDuration > 0 {
				go l.clearExpiredItems()
			}
		}
		l.mutex.Unlock()
	}
}

func (l *InMemoryRateLimiter) clearExpiredItems() {
	for {
		time.Sleep(l.expirationDuration)
		l.mutex.Lock()
		now := time.Now().Unix()
		for key := range l.store {
			queue := l.store[key]
			size := len(*queue)
			if size == 0 || now-(*queue)[size-1] > int64(l.expirationDuration.Seconds()) {
				delete(l.store, key)
			}
		}
		l.mutex.Unlock()
	}
}

// Request parameter duration's unit is seconds
func (l *InMemoryRateLimiter) Request(key string, maxRequestNum int, duration int64) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.requestLocked(key, maxRequestNum, duration)
}

func (l *InMemoryRateLimiter) AllowWithCheck(totalKey string, totalMax int, successKey string, successMax int, duration int64) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if successMax > 0 && !l.checkLocked(successKey, successMax, duration) {
		return false
	}
	if totalMax > 0 && !l.requestLocked(totalKey, totalMax, duration) {
		return false
	}
	return true
}

func (l *InMemoryRateLimiter) checkLocked(key string, maxRequestNum int, duration int64) bool {
	if maxRequestNum <= 0 {
		return true
	}
	queue, ok := l.store[key]
	now := time.Now().Unix()
	if ok {
		if len(*queue) < maxRequestNum {
			return true
		}
		if now-(*queue)[0] >= duration {
			return true
		}
		return false
	}
	return true
}

func (l *InMemoryRateLimiter) requestLocked(key string, maxRequestNum int, duration int64) bool {
	// [old <-- new]
	queue, ok := l.store[key]
	now := time.Now().Unix()
	if ok {
		if len(*queue) < maxRequestNum {
			*queue = append(*queue, now)
			return true
		} else {
			if now-(*queue)[0] >= duration {
				*queue = (*queue)[1:]
				*queue = append(*queue, now)
				return true
			} else {
				return false
			}
		}
	} else {
		s := make([]int64, 0, maxRequestNum)
		l.store[key] = &s
		*(l.store[key]) = append(*(l.store[key]), now)
	}
	return true
}
