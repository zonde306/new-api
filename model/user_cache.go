package model

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
)

const userCacheSchemaVersion = 2

type UserBase struct {
	Id          int    `json:"id"`
	Group       string `json:"group"`
	Email       string `json:"email"`
	Quota       int    `json:"quota"`
	Status      int    `json:"status"`
	Role        int    `json:"role"`
	Username    string `json:"username"`
	Setting     string `json:"setting"`
	AuthVersion int64  `json:"-"`
	CacheSchema int    `json:"-"`
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

func userCacheTTLSeconds() int {
	ttl := common.RedisKeyCacheSeconds()
	if ttl <= 0 {
		return 60
	}
	return ttl
}

// invalidateUserCache clears user cache
func invalidateUserCache(userId int) error {
	deleteUserBaseLocalCache(userId)
	if !common.RedisEnabled {
		return nil
	}
	return common.RedisDelKey(getUserCacheKey(userId))
}

// InvalidateUserCache is the exported version of invalidateUserCache.
// 供 controller 等上层包在用户状态变更（如禁用、删除、角色变更）后主动清理缓存。
func InvalidateUserCache(userId int) error {
	return invalidateUserCache(userId)
}

func populateUserCache(user User) error {
	if !common.RedisEnabled {
		return nil
	}
	return writeUserCache(user.ToBaseUser(), true)
}

// updateUserCache refreshes non-quota user cache fields.
// Quota is maintained by atomic quota delta paths and must not be overwritten
// by stale user snapshots from profile/settings updates.
func updateUserCache(user User) error {
	base := user.ToBaseUser()
	setUserBaseLocalCache(base)
	if !common.RedisEnabled {
		return nil
	}
	return writeUserCache(user.ToBaseUser(), false)
}

// GetUserCache gets complete user cache from memory -> redis -> db.
func GetUserCache(userId int) (*UserBase, error) {
	if userId <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}

	if common.MemoryCacheEnabled {
		if cached, ok := getUserBaseFromLocalCache(userId); ok {
			return cached, nil
		}
	}

	// Try getting from Redis first
	userCache, err := cacheGetUserBase(userId)
	if err == nil {
		setUserBaseLocalCache(userCache)
		return userCache, nil
	}

	// Redis misses and read failures both fall back to the shared database. A
	// version fence newer than the database is the one exception: allowing that
	// snapshot would re-authorize a user while a restrictive update is pending.
	user, err := GetUserById(userId, false)
	if err != nil {
		return nil, err
	}
	if common.RedisEnabled {
		floor, floorErr := getUserAuthVersionFloor(userId)
		if floorErr == nil && floor > user.AuthVersion {
			return nil, ErrUserAuthCachePending
		}
		if err := populateUserCache(*user); err != nil {
			if errors.Is(err, ErrUserAuthCachePending) {
				return nil, err
			}
			common.SysLog("failed to synchronously populate user cache: " + err.Error())
		}
	}
	base := user.ToBaseUser()
	setUserBaseLocalCache(base)
	return base, nil
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
	if userCache.Id != userId || userCache.CacheSchema != userCacheSchemaVersion || userCache.AuthVersion <= 0 {
		return nil, fmt.Errorf("user cache schema is stale")
	}
	floor, err := getUserAuthVersionFloor(userId)
	if err != nil {
		return nil, err
	}
	if floor > userCache.AuthVersion {
		return nil, ErrUserAuthCachePending
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
	return updateUserCacheField(userId, "Status", statusInt)
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

// RefreshUserGroupCache writes the database-authoritative group into an
// existing user hash without changing the user's authentication version.
func RefreshUserGroupCache(userId int) error {
	if userId <= 0 {
		return fmt.Errorf("invalid user id")
	}
	deleteUserBaseLocalCache(userId)
	if !common.RedisEnabled {
		return nil
	}
	var authoritative User
	if err := DB.Select("id", "auth_version", commonGroupCol).Where("id = ?", userId).First(&authoritative).Error; err != nil {
		return err
	}
	// Group transitions intentionally keep the same authentication version. A
	// refresh that read the previous group can therefore arrive after a newer
	// refresh and still pass the auth-version fence. Re-read after every write
	// and repair the cache when the authoritative group changed in between.
	for range 3 {
		if err := updateUserCacheFieldAtVersion(userId, "Group", authoritative.Group, authoritative.AuthVersion); err != nil {
			return err
		}

		var verified User
		if err := DB.Select("id", "auth_version", commonGroupCol).Where("id = ?", userId).First(&verified).Error; err != nil {
			return err
		}
		if verified.AuthVersion == authoritative.AuthVersion && verified.Group == authoritative.Group {
			return nil
		}
		authoritative = verified
	}

	// Preserve the freshest snapshot observed even when the row was too busy to
	// stabilize within the bounded retries. Returning an error lets best-effort
	// callers emit an operation-specific warning.
	if err := updateUserCacheFieldAtVersion(userId, "Group", authoritative.Group, authoritative.AuthVersion); err != nil {
		return err
	}
	return fmt.Errorf("user group changed repeatedly during cache refresh")
}

func updateUserEmailCache(userId int, email string) error {
	return updateUserCacheField(userId, "Email", email)
}

func updateUserNameCache(userId int, username string) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Username = username
	})
	return updateUserCacheField(userId, "Username", username)
}

func updateUserSettingCache(userId int, setting string) error {
	mutateUserBaseLocalCache(userId, func(cache *UserBase) {
		cache.Setting = setting
	})
	return updateUserCacheField(userId, "Setting", setting)
}

// updateUserCacheField prevents individual cache refreshes from bypassing the
// auth-version fence. It intentionally does nothing when the complete hash is
// absent; the next GetUserCache call will repopulate it from the database.
func updateUserCacheField(userId int, field string, value interface{}) error {
	if !common.RedisEnabled {
		return nil
	}
	var user User
	if err := DB.Select("id", "auth_version").Where("id = ?", userId).First(&user).Error; err != nil {
		return err
	}
	if user.AuthVersion <= 0 {
		return fmt.Errorf("invalid user auth version")
	}
	return updateUserCacheFieldAtVersion(userId, field, value, user.AuthVersion)
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
