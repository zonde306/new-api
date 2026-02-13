package middleware

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type ModelRequest struct {
	Model string `json:"model"`
	Group string `json:"group,omitempty"`
}

type modelRequestCacheEntry struct {
	ModelRequest         ModelRequest
	ShouldSelectChannel  bool
	RelayMode            int
	RelayModeSet         bool
	Platform             string
	TokenGroup           string
	TokenGroupSet        bool
	ExpireAtUnixNanoTime int64
}

var (
	modelRequestParseCache            = sync.Map{}
	modelRequestCacheEnabled          = common.GetEnvOrDefaultBool("ROUTING_PARSE_CACHE_ENABLED", true)
	modelRequestCacheTTL              = time.Duration(common.GetEnvOrDefault("ROUTING_PARSE_CACHE_TTL_SECONDS", 8)) * time.Second
	modelRequestCacheBodyMaxBytes     = int64(common.GetEnvOrDefault("ROUTING_PARSE_CACHE_BODY_MAX_BYTES", 1<<20))
	modelRequestCacheMaxQueryBytes    = int64(common.GetEnvOrDefault("ROUTING_PARSE_CACHE_MAX_QUERY_BYTES", 2048))
	modelRequestCacheMaxEntries       = int64(common.GetEnvOrDefault("ROUTING_PARSE_CACHE_MAX_ENTRIES", 20000))
	modelRequestCacheCleanupInterval  = time.Duration(common.GetEnvOrDefault("ROUTING_PARSE_CACHE_CLEANUP_INTERVAL_SECONDS", 15)) * time.Second
	modelRequestCacheEntryCount       = atomic.Int64{}
	modelRequestCacheCleanupRunning   = atomic.Bool{}
	modelRequestCacheLastCleanupNanos = atomic.Int64{}
	modelRequestWarmModels            = parseModelRequestWarmModels(common.GetEnvOrDefaultString("ROUTING_PARSE_CACHE_WARMUP_MODELS", "gpt-4o,gpt-4o-mini,gemini-2.0-flash"))
	modelRequestWarmModelSet          = buildModelRequestWarmModelSet(modelRequestWarmModels)
)

func init() {
	if !modelRequestCacheEnabled {
		return
	}
	if modelRequestCacheTTL <= 0 {
		modelRequestCacheTTL = 8 * time.Second
	}
	if modelRequestCacheBodyMaxBytes <= 0 {
		modelRequestCacheBodyMaxBytes = 1 << 20
	}
	if modelRequestCacheMaxQueryBytes <= 0 {
		modelRequestCacheMaxQueryBytes = 2048
	}
	if modelRequestCacheMaxEntries <= 0 {
		modelRequestCacheMaxEntries = 20000
	}
	if modelRequestCacheCleanupInterval <= 0 {
		modelRequestCacheCleanupInterval = 15 * time.Second
	}
	modelRequestCacheLastCleanupNanos.Store(time.Now().UnixNano())
	prewarmModelRequestParseCache()
	maybeCleanupModelRequestCache(true)
}

func parseModelRequestWarmModels(raw string) []string {
	parts := strings.Split(raw, ",")
	models := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		modelName := strings.TrimSpace(part)
		if modelName == "" {
			continue
		}
		if _, ok := seen[modelName]; ok {
			continue
		}
		seen[modelName] = struct{}{}
		models = append(models, modelName)
	}
	slices.Sort(models)
	return models
}

func buildModelRequestWarmModelSet(models []string) map[string]struct{} {
	warmSet := make(map[string]struct{}, len(models))
	for _, modelName := range models {
		if modelName == "" {
			continue
		}
		warmSet[modelName] = struct{}{}
	}
	return warmSet
}

func normalizeModelRequestContentType(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	return contentType
}

func isModelRequestWarmModel(modelName string) bool {
	if modelName == "" {
		return false
	}
	if strings.HasSuffix(modelName, ratio_setting.CompactModelSuffix) {
		modelName = strings.TrimSuffix(modelName, ratio_setting.CompactModelSuffix)
	}
	_, ok := modelRequestWarmModelSet[modelName]
	return ok
}

func modelRequestCacheTTLForModel(modelName string) time.Duration {
	if isModelRequestWarmModel(modelName) {
		return modelRequestCacheTTL * 3
	}
	return modelRequestCacheTTL
}

const modelRequestParseContextKey = "_routing_parse_cached_model_request"

func setModelRequestToParseContext(c *gin.Context, request ModelRequest) {
	if c == nil {
		return
	}
	c.Set(modelRequestParseContextKey, request)
}

func getModelRequestFromParseContext(c *gin.Context) (ModelRequest, bool) {
	if c == nil {
		return ModelRequest{}, false
	}
	raw, ok := c.Get(modelRequestParseContextKey)
	if !ok {
		return ModelRequest{}, false
	}
	request, ok := raw.(ModelRequest)
	if !ok {
		return ModelRequest{}, false
	}
	return request, true
}

func getModelRequestCacheTokenScope(c *gin.Context) string {
	if c == nil {
		return ""
	}
	raw, ok := common.GetContextKey(c, constant.ContextKeyTokenId)
	if !ok || raw == nil {
		return ""
	}
	var tokenScope string
	switch v := raw.(type) {
	case string:
		tokenScope = v
	case int:
		tokenScope = strconv.Itoa(v)
	case int8:
		tokenScope = strconv.FormatInt(int64(v), 10)
	case int16:
		tokenScope = strconv.FormatInt(int64(v), 10)
	case int32:
		tokenScope = strconv.FormatInt(int64(v), 10)
	case int64:
		tokenScope = strconv.FormatInt(v, 10)
	case uint:
		tokenScope = strconv.FormatUint(uint64(v), 10)
	case uint8:
		tokenScope = strconv.FormatUint(uint64(v), 10)
	case uint16:
		tokenScope = strconv.FormatUint(uint64(v), 10)
	case uint32:
		tokenScope = strconv.FormatUint(uint64(v), 10)
	case uint64:
		tokenScope = strconv.FormatUint(v, 10)
	default:
		tokenScope = fmt.Sprintf("%v", raw)
	}
	return strings.ReplaceAll(tokenScope, "|", "_")
}

func buildModelRequestCacheKeyFromBody(method, path, contentType, tokenScope string, body []byte) string {
	normalizedCT := normalizeModelRequestContentType(contentType)
	checksum := sha256.Sum256(body)
	return fmt.Sprintf("t=%s|m=%s|p=%s|ct=%s|l=%d|h=%x", tokenScope, method, path, normalizedCT, len(body), checksum)
}

func isModelRequestModelWarmPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/responses", "/v1/responses/compact":
		return true
	default:
		return false
	}
}

func normalizeModelNameForModelWarmCache(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	if strings.HasSuffix(modelName, ratio_setting.CompactModelSuffix) {
		modelName = strings.TrimSuffix(modelName, ratio_setting.CompactModelSuffix)
	}
	return modelName
}

func buildModelRequestWarmCacheKeyForModel(method, path, tokenScope, modelName string) string {
	return fmt.Sprintf("t=%s|m=%s|p=%s|wm=%s", tokenScope, method, path, modelName)
}

func extractModelNameForModelRequestWarmCache(c *gin.Context) (string, bool) {
	if c == nil || c.Request == nil {
		return "", false
	}
	contentType := normalizeModelRequestContentType(c.Request.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "json") {
		return "", false
	}
	if request, ok := getModelRequestFromParseContext(c); ok {
		modelName := normalizeModelNameForModelWarmCache(request.Model)
		if modelName == "" {
			return "", false
		}
		return modelName, true
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return "", false
	}
	if storage.Size() > modelRequestCacheBodyMaxBytes {
		return "", false
	}
	bodyBytes, err := storage.Bytes()
	if err != nil {
		return "", false
	}
	var request ModelRequest
	if err := common.Unmarshal(bodyBytes, &request); err != nil {
		return "", false
	}
	setModelRequestToParseContext(c, request)
	modelName := normalizeModelNameForModelWarmCache(request.Model)
	if modelName == "" {
		return "", false
	}
	return modelName, true
}

func buildModelRequestModelWarmCacheKeyWithTokenScope(c *gin.Context, tokenScope string, allowEmptyToken bool) (string, bool) {
	if !modelRequestCacheEnabled {
		return "", false
	}
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "", false
	}
	if !allowEmptyToken && tokenScope == "" {
		return "", false
	}
	method := c.Request.Method
	path := c.Request.URL.Path
	if method != http.MethodPost || !isModelRequestModelWarmPath(path) {
		return "", false
	}
	modelName, ok := extractModelNameForModelRequestWarmCache(c)
	if !ok {
		return "", false
	}
	return buildModelRequestWarmCacheKeyForModel(method, path, tokenScope, modelName), true
}

func buildModelRequestCacheKeyWithTokenScope(c *gin.Context, tokenScope string, allowEmptyToken bool) (string, bool) {
	if !modelRequestCacheEnabled {
		return "", false
	}
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "", false
	}
	if !allowEmptyToken && tokenScope == "" {
		return "", false
	}

	method := c.Request.Method
	path := c.Request.URL.Path
	if method == http.MethodGet {
		rawQuery := c.Request.URL.RawQuery
		if int64(len(rawQuery)) > modelRequestCacheMaxQueryBytes {
			return "", false
		}
		queryChecksum := sha256.Sum256([]byte(rawQuery))
		return fmt.Sprintf("t=%s|m=%s|p=%s|ql=%d|qh=%x", tokenScope, method, path, len(rawQuery), queryChecksum), true
	}

	if strings.Contains(path, "/suno/") ||
		(strings.Contains(path, "/v1/videos/") && strings.HasSuffix(path, "/remix")) ||
		strings.HasPrefix(path, "/v1beta/models/") ||
		strings.HasPrefix(path, "/v1/models/") {
		return fmt.Sprintf("t=%s|m=%s|p=%s", tokenScope, method, path), true
	}

	if method == http.MethodPost && isModelRequestModelWarmPath(path) {
		if modelWarmKey, ok := buildModelRequestModelWarmCacheKeyWithTokenScope(c, tokenScope, allowEmptyToken); ok {
			return modelWarmKey, true
		}
	}

	contentType := normalizeModelRequestContentType(c.Request.Header.Get("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		return "", false
	}

	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return "", false
	}
	if storage.Size() > modelRequestCacheBodyMaxBytes {
		return "", false
	}
	bodyBytes, err := storage.Bytes()
	if err != nil {
		return "", false
	}

	return buildModelRequestCacheKeyFromBody(method, path, contentType, tokenScope, bodyBytes), true
}

func buildModelRequestCacheKey(c *gin.Context) (string, bool) {
	tokenScope := getModelRequestCacheTokenScope(c)
	return buildModelRequestCacheKeyWithTokenScope(c, tokenScope, false)
}

func buildModelRequestModelWarmCacheKey(c *gin.Context) (string, bool) {
	return buildModelRequestModelWarmCacheKeyWithTokenScope(c, "", true)
}

func decreaseModelRequestCacheEntryCount(delta int64) {
	if delta <= 0 {
		return
	}
	for {
		current := modelRequestCacheEntryCount.Load()
		next := current - delta
		if next < 0 {
			next = 0
		}
		if modelRequestCacheEntryCount.CompareAndSwap(current, next) {
			return
		}
	}
}

func deleteModelRequestCacheByKey(cacheKey any) bool {
	if cacheKey == nil {
		return false
	}
	if _, loaded := modelRequestParseCache.LoadAndDelete(cacheKey); loaded {
		decreaseModelRequestCacheEntryCount(1)
		return true
	}
	return false
}

func maybeCleanupModelRequestCache(force bool) {
	nowNanos := time.Now().UnixNano()
	if !force {
		lastCleanup := modelRequestCacheLastCleanupNanos.Load()
		if lastCleanup > 0 && nowNanos-lastCleanup < int64(modelRequestCacheCleanupInterval) {
			return
		}
	}
	if !modelRequestCacheCleanupRunning.CompareAndSwap(false, true) {
		return
	}
	defer modelRequestCacheCleanupRunning.Store(false)

	nowNanos = time.Now().UnixNano()
	modelRequestCacheLastCleanupNanos.Store(nowNanos)
	modelRequestParseCache.Range(func(key, value any) bool {
		entry, ok := value.(*modelRequestCacheEntry)
		if !ok || entry == nil || nowNanos > entry.ExpireAtUnixNanoTime {
			deleteModelRequestCacheByKey(key)
		}
		return true
	})
}

func getModelRequestCache(cacheKey string) (*modelRequestCacheEntry, bool) {
	if cacheKey == "" {
		return nil, false
	}
	maybeCleanupModelRequestCache(false)
	cached, ok := modelRequestParseCache.Load(cacheKey)
	if !ok {
		return nil, false
	}
	entry, ok := cached.(*modelRequestCacheEntry)
	if !ok || entry == nil {
		deleteModelRequestCacheByKey(cacheKey)
		return nil, false
	}
	if time.Now().UnixNano() > entry.ExpireAtUnixNanoTime {
		deleteModelRequestCacheByKey(cacheKey)
		return nil, false
	}
	return entry, true
}

func setModelRequestCache(cacheKey string, entry *modelRequestCacheEntry) {
	if cacheKey == "" || entry == nil {
		return
	}
	maybeCleanupModelRequestCache(false)
	ttl := modelRequestCacheTTLForModel(entry.ModelRequest.Model)
	entry.ExpireAtUnixNanoTime = time.Now().Add(ttl).UnixNano()

	for {
		if modelRequestCacheEntryCount.Load() >= modelRequestCacheMaxEntries {
			maybeCleanupModelRequestCache(true)
			if modelRequestCacheEntryCount.Load() >= modelRequestCacheMaxEntries {
				return
			}
		}
		existingValue, loaded := modelRequestParseCache.LoadOrStore(cacheKey, entry)
		if !loaded {
			modelRequestCacheEntryCount.Add(1)
			return
		}
		if modelRequestParseCache.CompareAndSwap(cacheKey, existingValue, entry) {
			return
		}
		// 并发下 key 可能在 LoadOrStore 与更新之间被删除或替换，重试可避免计数漂移。
	}
}

func buildModelRequestCacheEntryFromContext(c *gin.Context, modelRequest *ModelRequest, shouldSelectChannel bool) *modelRequestCacheEntry {
	if modelRequest == nil {
		return nil
	}
	entry := &modelRequestCacheEntry{
		ModelRequest:        *modelRequest,
		ShouldSelectChannel: shouldSelectChannel,
	}
	if relayModeRaw, ok := c.Get("relay_mode"); ok {
		if relayMode, castOk := relayModeRaw.(int); castOk {
			entry.RelayMode = relayMode
			entry.RelayModeSet = true
		}
	}
	if platformRaw, ok := c.Get("platform"); ok {
		if platform, castOk := platformRaw.(string); castOk {
			entry.Platform = platform
		}
	}
	if tokenGroupRaw, ok := common.GetContextKey(c, constant.ContextKeyTokenGroup); ok {
		if tokenGroup, castOk := tokenGroupRaw.(string); castOk {
			entry.TokenGroup = tokenGroup
			entry.TokenGroupSet = true
		}
	}
	return entry
}

func applyModelRequestCacheEntry(c *gin.Context, entry *modelRequestCacheEntry) {
	if c == nil || entry == nil {
		return
	}
	if entry.RelayModeSet {
		c.Set("relay_mode", entry.RelayMode)
	}
	if entry.Platform != "" {
		c.Set("platform", entry.Platform)
	}
	if entry.TokenGroupSet {
		common.SetContextKey(c, constant.ContextKeyTokenGroup, entry.TokenGroup)
	}
}

func prewarmModelRequestParseCache() {
	if len(modelRequestWarmModels) == 0 {
		return
	}
	modelWarmPaths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/responses",
		"/v1/responses/compact",
	}

	for _, modelName := range modelRequestWarmModels {
		normalizedModelName := normalizeModelNameForModelWarmCache(modelName)
		if normalizedModelName == "" {
			continue
		}
		for _, path := range modelWarmPaths {
			warmedModelName := normalizedModelName
			if path == "/v1/responses/compact" {
				warmedModelName = ratio_setting.WithCompactModelSuffix(normalizedModelName)
			}
			cacheKey := buildModelRequestWarmCacheKeyForModel(http.MethodPost, path, "", normalizedModelName)
			setModelRequestCache(cacheKey, &modelRequestCacheEntry{
				ModelRequest:        ModelRequest{Model: warmedModelName},
				ShouldSelectChannel: true,
			})
		}
	}
}

func Distribute() func(c *gin.Context) {
	return func(c *gin.Context) {
		var channel *model.Channel
		channelId, ok := common.GetContextKey(c, constant.ContextKeyTokenSpecificChannelId)
		modelRequest, shouldSelectChannel, err := getModelRequest(c)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidRequest, map[string]any{"Error": err.Error()}))
			return
		}
		if ok {
			id, err := strconv.Atoi(channelId.(string))
			if err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidChannelId))
				return
			}
			channel, err = model.GetChannelById(id, true)
			if err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidChannelId))
				return
			}
			if channel.Status != common.ChannelStatusEnabled {
				abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorChannelDisabled))
				return
			}
		} else {
			// Select a channel for the user
			// check token model mapping
			modelLimitEnable := common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled)
			if modelLimitEnable {
				s, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
				if !ok {
					// token model limit is empty, all models are not allowed
					abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenNoModelAccess))
					return
				}
				var tokenModelLimit map[string]bool
				tokenModelLimit, ok = s.(map[string]bool)
				if !ok {
					tokenModelLimit = map[string]bool{}
				}
				matchName := ratio_setting.FormatMatchingModelName(modelRequest.Model) // match gpts & thinking-*
				if _, ok := tokenModelLimit[matchName]; !ok {
					abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenModelForbidden, map[string]any{"Model": modelRequest.Model}))
					return
				}
			}

			if shouldSelectChannel {
				if modelRequest.Model == "" {
					abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorModelNameRequired))
					return
				}
				var selectGroup string
				usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
				// check path is /pg/chat/completions
				if strings.HasPrefix(c.Request.URL.Path, "/pg/chat/completions") {
					playgroundRequest := &dto.PlayGroundRequest{}
					err = common.UnmarshalBodyReusable(c, playgroundRequest)
					if err != nil {
						abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidPlayground, map[string]any{"Error": err.Error()}))
						return
					}
					if playgroundRequest.Group != "" {
						if !service.GroupInUserUsableGroups(usingGroup, playgroundRequest.Group) && playgroundRequest.Group != usingGroup {
							abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorGroupAccessDenied))
							return
						}
						usingGroup = playgroundRequest.Group
						common.SetContextKey(c, constant.ContextKeyUsingGroup, usingGroup)
					}
				}

				if preferredChannelID, found := service.GetPreferredChannelByAffinity(c, modelRequest.Model, usingGroup); found {
					preferred, err := model.CacheGetChannel(preferredChannelID)
					if err == nil && preferred != nil && preferred.Status == common.ChannelStatusEnabled {
						if usingGroup == "auto" {
							userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
							autoGroups := service.GetUserAutoGroup(userGroup)
							for _, g := range autoGroups {
								if model.IsChannelEnabledForGroupModel(g, modelRequest.Model, preferred.Id) {
									selectGroup = g
									common.SetContextKey(c, constant.ContextKeyAutoGroup, g)
									channel = preferred
									service.MarkChannelAffinityUsed(c, g, preferred.Id)
									break
								}
							}
						} else if model.IsChannelEnabledForGroupModel(usingGroup, modelRequest.Model, preferred.Id) {
							channel = preferred
							selectGroup = usingGroup
							service.MarkChannelAffinityUsed(c, usingGroup, preferred.Id)
						}
					}
				}

				if channel == nil {
					channel, selectGroup, err = service.CacheGetRandomSatisfiedChannel(&service.RetryParam{
						Ctx:        c,
						ModelName:  modelRequest.Model,
						TokenGroup: usingGroup,
						Retry:      common.GetPointer(0),
					})
					if err != nil {
						showGroup := usingGroup
						if usingGroup == "auto" {
							showGroup = fmt.Sprintf("auto(%s)", selectGroup)
						}
						message := i18n.T(c, i18n.MsgDistributorGetChannelFailed, map[string]any{"Group": showGroup, "Model": modelRequest.Model, "Error": err.Error()})
						// 如果错误，但是渠道不为空，说明是数据库一致性问题
						//if channel != nil {
						//	common.SysError(fmt.Sprintf("渠道不存在：%d", channel.Id))
						//	message = "数据库一致性已被破坏，请联系管理员"
						//}
						abortWithOpenAiMessage(c, http.StatusServiceUnavailable, message, types.ErrorCodeModelNotFound)
						return
					}
					if channel == nil {
						abortWithOpenAiMessage(c, http.StatusServiceUnavailable, i18n.T(c, i18n.MsgDistributorNoAvailableChannel, map[string]any{"Group": usingGroup, "Model": modelRequest.Model}), types.ErrorCodeModelNotFound)
						return
					}
				}
			}
		}
		common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
		SetupContextForSelectedChannel(c, channel, modelRequest.Model)
		c.Next()
		if channel != nil && c.Writer != nil && c.Writer.Status() < http.StatusBadRequest {
			service.RecordChannelAffinity(c, channel.Id)
		}
	}
}

// getModelFromRequest 从请求中读取模型信息
// 根据 Content-Type 自动处理：
// - application/json
// - application/x-www-form-urlencoded
// - multipart/form-data
func getModelFromRequest(c *gin.Context) (*ModelRequest, error) {
	if cachedModelRequest, ok := getModelRequestFromParseContext(c); ok {
		modelRequest := cachedModelRequest
		return &modelRequest, nil
	}
	var modelRequest ModelRequest
	err := common.UnmarshalBodyReusable(c, &modelRequest)
	if err != nil {
		return nil, errors.New(i18n.T(c, i18n.MsgDistributorInvalidRequest, map[string]any{"Error": err.Error()}))
	}
	setModelRequestToParseContext(c, modelRequest)
	return &modelRequest, nil
}

func getModelRequest(c *gin.Context) (*ModelRequest, bool, error) {
	cacheKey, cacheEnabled := buildModelRequestCacheKey(c)
	if cacheEnabled {
		if entry, ok := getModelRequestCache(cacheKey); ok {
			modelRequest := entry.ModelRequest
			applyModelRequestCacheEntry(c, entry)
			return &modelRequest, entry.ShouldSelectChannel, nil
		}
		if modelWarmKey, warmModelEnabled := buildModelRequestModelWarmCacheKey(c); warmModelEnabled && modelWarmKey != cacheKey {
			if entry, ok := getModelRequestCache(modelWarmKey); ok {
				modelRequest := entry.ModelRequest
				applyModelRequestCacheEntry(c, entry)
				return &modelRequest, entry.ShouldSelectChannel, nil
			}
		}
	}

	var modelRequest ModelRequest
	shouldSelectChannel := true
	var err error
	if strings.Contains(c.Request.URL.Path, "/mj/") {
		relayMode := relayconstant.Path2RelayModeMidjourney(c.Request.URL.Path)
		if relayMode == relayconstant.RelayModeMidjourneyTaskFetch ||
			relayMode == relayconstant.RelayModeMidjourneyTaskFetchByCondition ||
			relayMode == relayconstant.RelayModeMidjourneyNotify ||
			relayMode == relayconstant.RelayModeMidjourneyTaskImageSeed {
			shouldSelectChannel = false
		} else {
			midjourneyRequest := dto.MidjourneyRequest{}
			err = common.UnmarshalBodyReusable(c, &midjourneyRequest)
			if err != nil {
				return nil, false, errors.New(i18n.T(c, i18n.MsgDistributorInvalidMidjourney, map[string]any{"Error": err.Error()}))
			}
			midjourneyModel, mjErr, success := service.GetMjRequestModel(relayMode, &midjourneyRequest)
			if mjErr != nil {
				return nil, false, fmt.Errorf("%s", mjErr.Description)
			}
			if midjourneyModel == "" {
				if !success {
					return nil, false, fmt.Errorf("%s", i18n.T(c, i18n.MsgDistributorInvalidParseModel))
				} else {
					// task fetch, task fetch by condition, notify
					shouldSelectChannel = false
				}
			}
			modelRequest.Model = midjourneyModel
		}
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/suno/") {
		relayMode := relayconstant.Path2RelaySuno(c.Request.Method, c.Request.URL.Path)
		if relayMode == relayconstant.RelayModeSunoFetch ||
			relayMode == relayconstant.RelayModeSunoFetchByID {
			shouldSelectChannel = false
		} else {
			modelName := service.CoverTaskActionToModelName(constant.TaskPlatformSuno, c.Param("action"))
			modelRequest.Model = modelName
		}
		c.Set("platform", string(constant.TaskPlatformSuno))
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/v1/videos/") && strings.HasSuffix(c.Request.URL.Path, "/remix") {
		relayMode := relayconstant.RelayModeVideoSubmit
		c.Set("relay_mode", relayMode)
		shouldSelectChannel = false
	} else if strings.Contains(c.Request.URL.Path, "/v1/videos") {
		//curl https://api.openai.com/v1/videos \
		//  -H "Authorization: Bearer $OPENAI_API_KEY" \
		//  -F "model=sora-2" \
		//  -F "prompt=A calico cat playing a piano on stage"
		//	-F input_reference="@image.jpg"
		relayMode := relayconstant.RelayModeUnknown
		if c.Request.Method == http.MethodPost {
			relayMode = relayconstant.RelayModeVideoSubmit
			req, err := getModelFromRequest(c)
			if err != nil {
				return nil, false, err
			}
			if req != nil {
				modelRequest.Model = req.Model
			}
		} else if c.Request.Method == http.MethodGet {
			relayMode = relayconstant.RelayModeVideoFetchByID
			shouldSelectChannel = false
		}
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/v1/video/generations") {
		relayMode := relayconstant.RelayModeUnknown
		if c.Request.Method == http.MethodPost {
			req, err := getModelFromRequest(c)
			if err != nil {
				return nil, false, err
			}
			modelRequest.Model = req.Model
			relayMode = relayconstant.RelayModeVideoSubmit
		} else if c.Request.Method == http.MethodGet {
			relayMode = relayconstant.RelayModeVideoFetchByID
			shouldSelectChannel = false
		}
		if _, ok := c.Get("relay_mode"); !ok {
			c.Set("relay_mode", relayMode)
		}
	} else if strings.HasPrefix(c.Request.URL.Path, "/v1beta/models/") || strings.HasPrefix(c.Request.URL.Path, "/v1/models/") {
		// Gemini API 路径处理: /v1beta/models/gemini-2.0-flash:generateContent
		relayMode := relayconstant.RelayModeGemini
		modelName := extractModelNameFromGeminiPath(c.Request.URL.Path)
		if modelName != "" {
			modelRequest.Model = modelName
		}
		c.Set("relay_mode", relayMode)
	} else if !strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") && !strings.Contains(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
		req, err := getModelFromRequest(c)
		if err != nil {
			return nil, false, err
		}
		modelRequest.Model = req.Model
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/realtime") {
		//wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview-2024-10-01
		modelRequest.Model = c.Query("model")
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/moderations") {
		if modelRequest.Model == "" {
			modelRequest.Model = "text-moderation-stable"
		}
	}
	if strings.HasSuffix(c.Request.URL.Path, "embeddings") {
		if modelRequest.Model == "" {
			modelRequest.Model = c.Param("model")
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/images/generations") {
		modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "dall-e")
	} else if strings.HasPrefix(c.Request.URL.Path, "/v1/images/edits") {
		//modelRequest.Model = common.GetStringIfEmpty(c.PostForm("model"), "gpt-image-1")
		contentType := c.ContentType()
		if slices.Contains([]string{gin.MIMEPOSTForm, gin.MIMEMultipartPOSTForm}, contentType) {
			req, err := getModelFromRequest(c)
			if err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/audio") {
		relayMode := relayconstant.RelayModeAudioSpeech
		if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/speech") {

			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "tts-1")
		} else if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/translations") {
			// 先尝试从请求读取
			if req, err := getModelFromRequest(c); err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "whisper-1")
			relayMode = relayconstant.RelayModeAudioTranslation
		} else if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") {
			// 先尝试从请求读取
			if req, err := getModelFromRequest(c); err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "whisper-1")
			relayMode = relayconstant.RelayModeAudioTranscription
		}
		c.Set("relay_mode", relayMode)
	}
	if strings.HasPrefix(c.Request.URL.Path, "/pg/chat/completions") {
		// playground chat completions
		req, err := getModelFromRequest(c)
		if err != nil {
			return nil, false, err
		}
		modelRequest.Model = req.Model
		modelRequest.Group = req.Group
		common.SetContextKey(c, constant.ContextKeyTokenGroup, modelRequest.Group)
	}

	if strings.HasPrefix(c.Request.URL.Path, "/v1/responses/compact") && modelRequest.Model != "" {
		modelRequest.Model = ratio_setting.WithCompactModelSuffix(modelRequest.Model)
	}

	result := &modelRequest
	if cacheEnabled {
		setModelRequestCache(cacheKey, buildModelRequestCacheEntryFromContext(c, result, shouldSelectChannel))
	}
	return result, shouldSelectChannel, nil
}

func SetupContextForSelectedChannel(c *gin.Context, channel *model.Channel, modelName string) *types.NewAPIError {
	c.Set("original_model", modelName) // for retry
	if channel == nil {
		return types.NewError(errors.New("channel is nil"), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
	common.SetContextKey(c, constant.ContextKeyChannelName, channel.Name)
	common.SetContextKey(c, constant.ContextKeyChannelType, channel.Type)
	common.SetContextKey(c, constant.ContextKeyChannelCreateTime, channel.CreatedTime)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, channel.GetSetting())
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, channel.GetOtherSettings())
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, channel.GetParamOverride())
	common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, channel.GetHeaderOverride())
	if nil != channel.OpenAIOrganization && *channel.OpenAIOrganization != "" {
		common.SetContextKey(c, constant.ContextKeyChannelOrganization, *channel.OpenAIOrganization)
	}
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, channel.GetAutoBan())
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, channel.GetModelMapping())
	common.SetContextKey(c, constant.ContextKeyChannelStatusCodeMapping, channel.GetStatusCodeMapping())

	key, index, newAPIError := channel.GetNextEnabledKey()
	if newAPIError != nil {
		return newAPIError
	}
	if channel.ChannelInfo.IsMultiKey {
		common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, true)
		common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, index)
	} else {
		// 必须设置为 false，否则在重试到单个 key 的时候会导致日志显示错误
		common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, false)
	}
	// c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", key))
	common.SetContextKey(c, constant.ContextKeyChannelKey, key)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, channel.GetBaseURL())

	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, false)

	// TODO: api_version统一
	switch channel.Type {
	case constant.ChannelTypeAzure:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeVertexAi:
		c.Set("region", channel.Other)
	case constant.ChannelTypeXunfei:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeGemini:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeAli:
		c.Set("plugin", channel.Other)
	case constant.ChannelCloudflare:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeMokaAI:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeCoze:
		c.Set("bot_id", channel.Other)
	}
	return nil
}

// extractModelNameFromGeminiPath 从 Gemini API URL 路径中提取模型名
// 输入格式: /v1beta/models/gemini-2.0-flash:generateContent
// 输出: gemini-2.0-flash
func extractModelNameFromGeminiPath(path string) string {
	// 查找 "/models/" 的位置
	modelsPrefix := "/models/"
	modelsIndex := strings.Index(path, modelsPrefix)
	if modelsIndex == -1 {
		return ""
	}

	// 从 "/models/" 之后开始提取
	startIndex := modelsIndex + len(modelsPrefix)
	if startIndex >= len(path) {
		return ""
	}

	// 查找 ":" 的位置，模型名在 ":" 之前
	colonIndex := strings.Index(path[startIndex:], ":")
	if colonIndex == -1 {
		// 如果没有找到 ":"，返回从 "/models/" 到路径结尾的部分
		return path[startIndex:]
	}

	// 返回模型名部分
	return path[startIndex : startIndex+colonIndex]
}
