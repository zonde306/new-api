package model

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"

	"github.com/bytedance/gopkg/util/gopool"
)

// UserBase struct remains the same as it represents the cached data structure
type UserBase struct {
	Id       int    `json:"id"`
	Group    string `json:"group"`
	Email    string `json:"email"`
	Quota    int    `json:"quota"`
	Status   int    `json:"status"`
	Username string `json:"username"`
	Setting  string `json:"setting"`
}

type userBaseLocalCacheEntry struct {
	Value            UserBase
	ExpireAtUnixNano int64
}

const userBaseLocalLockShardCount = 256

var (
	userBaseLocalCache                sync.Map // map[int]userBaseLocalCacheEntry
	userBaseLocalCacheTTL             = time.Duration(common.GetEnvOrDefault("USER_BASE_LOCAL_CACHE_TTL_SECONDS", 5)) * time.Second
	userBaseLocalCacheCleanupInterval = time.Duration(common.GetEnvOrDefault("USER_BASE_LOCAL_CACHE_CLEANUP_SECONDS", 60)) * time.Second
	userBaseLocalLocks                [userBaseLocalLockShardCount]sync.Mutex
	userBaseLocalJanitorStartOnce     sync.Once
	userBaseLocalJanitorStopOnce      sync.Once
	userBaseLocalJanitorStopCh        = make(chan struct{})
)

func init() {
	if userBaseLocalCacheTTL <= 0 {
		userBaseLocalCacheTTL = 5 * time.Second
	}
	if userBaseLocalCacheCleanupInterval <= 0 {
		userBaseLocalCacheCleanupInterval = 60 * time.Second
	}
	if userBaseLocalCacheCleanupInterval > userBaseLocalCacheTTL {
		userBaseLocalCacheCleanupInterval = userBaseLocalCacheTTL
	}
}

func ensureUserBaseLocalCacheJanitor() {
	if !common.MemoryCacheEnabled {
		return
	}
	startUserBaseLocalCacheJanitor()
}

func startUserBaseLocalCacheJanitor() {
	userBaseLocalJanitorStartOnce.Do(func() {
		ticker := time.NewTicker(userBaseLocalCacheCleanupInterval)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					cleanupExpiredUserBaseLocalCache(time.Now().UnixNano())
				case <-userBaseLocalJanitorStopCh:
					return
				}
			}
		}()
	})
}

func stopUserBaseLocalCacheJanitor() {
	userBaseLocalJanitorStopOnce.Do(func() {
		close(userBaseLocalJanitorStopCh)
	})
}

func cleanupExpiredUserBaseLocalCache(nowUnixNano int64) {
	userBaseLocalCache.Range(func(key, value any) bool {
		entry, ok := value.(userBaseLocalCacheEntry)
		if !ok || nowUnixNano > entry.ExpireAtUnixNano {
			userBaseLocalCache.Delete(key)
		}
		return true
	})
}

func getUserBaseShardLock(userId int) *sync.Mutex {
	idx := userId % userBaseLocalLockShardCount
	if idx < 0 {
		idx = -idx
	}
	return &userBaseLocalLocks[idx]
}

func getUserBaseFromLocalCache(userId int) (*UserBase, bool) {
	if !common.MemoryCacheEnabled || userId <= 0 {
		return nil, false
	}
	ensureUserBaseLocalCacheJanitor()
	raw, ok := userBaseLocalCache.Load(userId)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(userBaseLocalCacheEntry)
	if !ok {
		userBaseLocalCache.Delete(userId)
		return nil, false
	}
	if time.Now().UnixNano() > entry.ExpireAtUnixNano {
		userBaseLocalCache.Delete(userId)
		return nil, false
	}
	cached := entry.Value
	return &cached, true
}

func setUserBaseLocalCacheNoLock(userCache *UserBase) {
	ttl := userBaseLocalCacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	userBaseLocalCache.Store(userCache.Id, userBaseLocalCacheEntry{
		Value:            *userCache,
		ExpireAtUnixNano: time.Now().Add(ttl).UnixNano(),
	})
}

func setUserBaseLocalCache(userCache *UserBase) {
	if !common.MemoryCacheEnabled || userCache == nil || userCache.Id <= 0 {
		return
	}
	ensureUserBaseLocalCacheJanitor()
	lock := getUserBaseShardLock(userCache.Id)
	lock.Lock()
	defer lock.Unlock()
	setUserBaseLocalCacheNoLock(userCache)
}

func deleteUserBaseLocalCache(userId int) {
	if userId <= 0 {
		return
	}
	lock := getUserBaseShardLock(userId)
	lock.Lock()
	defer lock.Unlock()
	userBaseLocalCache.Delete(userId)
}

func mutateUserBaseLocalCache(userId int, mutate func(*UserBase)) {
	if !common.MemoryCacheEnabled || userId <= 0 || mutate == nil {
		return
	}
	ensureUserBaseLocalCacheJanitor()
	lock := getUserBaseShardLock(userId)
	lock.Lock()
	defer lock.Unlock()

	raw, ok := userBaseLocalCache.Load(userId)
	if !ok {
		return
	}
	entry, ok := raw.(userBaseLocalCacheEntry)
	if !ok {
		userBaseLocalCache.Delete(userId)
		return
	}
	if time.Now().UnixNano() > entry.ExpireAtUnixNano {
		userBaseLocalCache.Delete(userId)
		return
	}
	next := entry.Value
	mutate(&next)
	entry.Value = next
	userBaseLocalCache.Store(userId, entry)
}

func (user *UserBase) WriteContext(c *gin.Context) {
	common.SetContextKey(c, constant.ContextKeyUserGroup, user.Group)
	common.SetContextKey(c, constant.ContextKeyUserQuota, user.Quota)
	common.SetContextKey(c, constant.ContextKeyUserStatus, user.Status)
	common.SetContextKey(c, constant.ContextKeyUserEmail, user.Email)
	common.SetContextKey(c, constant.ContextKeyUserName, user.Username)
	common.SetContextKey(c, constant.ContextKeyUserSetting, user.GetSetting())
}

func (user *UserBase) GetSetting() dto.UserSetting {
	setting := dto.UserSetting{}
	if user.Setting != "" {
		err := common.Unmarshal([]byte(user.Setting), &setting)
		if err != nil {
			common.SysLog("failed to unmarshal setting: " + err.Error())
		}
	}
	return setting
}

// getUserCacheKey returns the key for user cache
func getUserCacheKey(userId int) string {
	return fmt.Sprintf("user:%d", userId)
}

// invalidateUserCache clears user cache
func invalidateUserCache(userId int) error {
	deleteUserBaseLocalCache(userId)
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisDelKey(getUserCacheKey(userId))
}

// updateUserCache updates all user cache fields using hash
func updateUserCache(user User) error {
	base := user.ToBaseUser()
	setUserBaseLocalCache(base)
	if !common.RedisEnabled {
		return nil
	}

	return common.RedisHSetObj(
		getUserCacheKey(user.Id),
		base,
		time.Duration(common.RedisKeyCacheSeconds())*time.Second,
	)
}

// GetUserCache gets complete user cache from memory -> redis -> db.
func GetUserCache(userId int) (userCache *UserBase, err error) {
	if userId <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}

	if common.MemoryCacheEnabled {
		if cached, ok := getUserBaseFromLocalCache(userId); ok {
			return cached, nil
		}
	}

	var user *User
	var fromDB bool
	defer func() {
		// Update Redis cache asynchronously on successful DB read
		if shouldUpdateRedis(fromDB, err) && user != nil {
			gopool.Go(func() {
				if err := updateUserCache(*user); err != nil {
					common.SysLog("failed to update user cache: " + err.Error())
				}
			})
		}
	}()

	if common.RedisEnabled {
		userCache, err = cacheGetUserBase(userId)
		if err == nil {
			setUserBaseLocalCache(userCache)
			return userCache, nil
		}
	}

	fromDB = true
	user, err = GetUserById(userId, false)
	if err != nil {
		return nil, err
	}

	userCache = &UserBase{
		Id:       user.Id,
		Group:    user.Group,
		Quota:    user.Quota,
		Status:   user.Status,
		Username: user.Username,
		Setting:  user.Setting,
		Email:    user.Email,
	}
	setUserBaseLocalCache(userCache)
	return userCache, nil
}

func cacheGetUserBase(userId int) (*UserBase, error) {
	if !common.RedisEnabled {
		return nil, fmt.Errorf("redis is not enabled")
	}
	var userCache UserBase
	// Try getting from Redis first
	err := common.RedisHGetObj(getUserCacheKey(userId), &userCache)
	if err != nil {
		return nil, err
	}
	return &userCache, nil
}

// Add atomic quota operations using hash fields
func incrUserBaseLocalQuotaCache(userId int, delta int) {
	if delta == 0 {
		return
	}
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Quota += delta
	})
}

func cacheIncrUserQuota(userId int, delta int64) error {
	if delta == 0 {
		return nil
	}
	if common.RedisEnabled {
		if err := common.RedisHIncrBy(getUserCacheKey(userId), "Quota", delta); err != nil {
			deleteUserBaseLocalCache(userId)
			return err
		}
	}
	incrUserBaseLocalQuotaCache(userId, int(delta))
	return nil
}

func cacheDecrUserQuota(userId int, delta int64) error {
	return cacheIncrUserQuota(userId, -delta)
}

// Helper functions to get individual fields if needed
func getUserGroupCache(userId int) (string, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return "", err
	}
	return cache.Group, nil
}

func getUserQuotaCache(userId int) (int, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return 0, err
	}
	return cache.Quota, nil
}

func getUserStatusCache(userId int) (int, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return 0, err
	}
	return cache.Status, nil
}

func getUserNameCache(userId int) (string, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return "", err
	}
	return cache.Username, nil
}

func getUserSettingCache(userId int) (dto.UserSetting, error) {
	cache, err := GetUserCache(userId)
	if err != nil {
		return dto.UserSetting{}, err
	}
	return cache.GetSetting(), nil
}

// New functions for individual field updates
func updateUserStatusCache(userId int, status bool) error {
	statusInt := common.UserStatusEnabled
	if !status {
		statusInt = common.UserStatusDisabled
	}
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Status = statusInt
	})
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisHSetField(getUserCacheKey(userId), "Status", fmt.Sprintf("%d", statusInt))
}

func updateUserQuotaCache(userId int, quota int) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Quota = quota
	})
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisHSetField(getUserCacheKey(userId), "Quota", fmt.Sprintf("%d", quota))
}

func updateUserGroupCache(userId int, group string) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Group = group
	})
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisHSetField(getUserCacheKey(userId), "Group", group)
}

func UpdateUserGroupCache(userId int, group string) error {
	return updateUserGroupCache(userId, group)
}

func updateUserNameCache(userId int, username string) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Username = username
	})
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisHSetField(getUserCacheKey(userId), "Username", username)
}

func updateUserSettingCache(userId int, setting string) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Setting = setting
	})
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisHSetField(getUserCacheKey(userId), "Setting", setting)
}

// GetUserLanguage returns the user's language preference from cache
// Uses the existing GetUserCache mechanism for efficiency
func GetUserLanguage(userId int) string {
	userCache, err := GetUserCache(userId)
	if err != nil {
		return ""
	}
	return userCache.GetSetting().Language
}
