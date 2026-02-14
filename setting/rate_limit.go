package setting

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

var ModelRequestRateLimitEnabled = false
var ModelRequestRateLimitDurationMinutes = 1
var ModelRequestRateLimitCount = 0
var ModelRequestRateLimitSuccessCount = 1000

// 兼容语法：
// 1) 旧语法：{"group": [total, success]}
// 2) 新语法：{"user_group": {"token_group": [total, success]}}
var ModelRequestRateLimitGroup = map[string][2]int{}
var ModelRequestRateLimitByUserTokenGroup = map[string]map[string][2]int{}

// 基于 IP 的模型请求速率限制扩展
var ModelRequestIPRateLimitEnabled = false
var ModelRequestIPRateLimitDurationMinutes = 1
var ModelRequestIPRateLimitUserCount = 0
var ModelRequestIPRateLimitUserSuccessCount = 0
var ModelRequestIPRateLimitGroup = map[string][2]int{}
var ModelRequestIPRateLimitByUserTokenGroup = map[string]map[string][2]int{}

var ModelRequestRateLimitMutex sync.RWMutex

func mergeRateLimitGroups(simple map[string][2]int, byUserToken map[string]map[string][2]int) map[string]any {
	result := make(map[string]any)
	for group, limits := range simple {
		result[group] = limits
	}
	for userGroup, tokenGroups := range byUserToken {
		tokenGroupMap := make(map[string][2]int)
		for tokenGroup, limits := range tokenGroups {
			tokenGroupMap[tokenGroup] = limits
		}
		result[userGroup] = tokenGroupMap
	}
	return result
}

func ModelRequestRateLimitGroup2JSONString() string {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	jsonBytes, err := common.Marshal(mergeRateLimitGroups(ModelRequestRateLimitGroup, ModelRequestRateLimitByUserTokenGroup))
	if err != nil {
		common.SysLog("error marshalling model ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func parseRateLimitValueToInt(v any) (int, error) {
	// 先做范围检查再转换，避免极大值在转换为 int 时出现实现相关行为
	validateRange := func(num float64) error {
		if num < math.MinInt32 || num > math.MaxInt32 {
			return fmt.Errorf("rate limit value %.6f out of allowed range [%d, %d]", num, math.MinInt32, math.MaxInt32)
		}
		return nil
	}

	switch val := v.(type) {
	case float64:
		if math.Trunc(val) != val {
			return 0, fmt.Errorf("rate limit value %.6f is not integer", val)
		}
		if err := validateRange(val); err != nil {
			return 0, err
		}
		return int(val), nil
	case int:
		if err := validateRange(float64(val)); err != nil {
			return 0, err
		}
		return val, nil
	case int32:
		if err := validateRange(float64(val)); err != nil {
			return 0, err
		}
		return int(val), nil
	case int64:
		if err := validateRange(float64(val)); err != nil {
			return 0, err
		}
		return int(val), nil
	case json.Number:
		i64, err := val.Int64()
		if err != nil {
			return 0, fmt.Errorf("invalid json number %s", val.String())
		}
		if err := validateRange(float64(i64)); err != nil {
			return 0, err
		}
		return int(i64), nil
	default:
		return 0, fmt.Errorf("invalid rate limit value type %T", v)
	}
}

func parseRateLimitPair(raw any) ([2]int, error) {
	var limits [2]int
	arr, ok := raw.([]any)
	if !ok {
		return limits, fmt.Errorf("rate limit value must be [total, success], got %T", raw)
	}
	if len(arr) != 2 {
		return limits, fmt.Errorf("rate limit value must have exactly 2 items, got %d", len(arr))
	}
	total, err := parseRateLimitValueToInt(arr[0])
	if err != nil {
		return limits, err
	}
	success, err := parseRateLimitValueToInt(arr[1])
	if err != nil {
		return limits, err
	}
	limits[0] = total
	limits[1] = success
	return limits, nil
}

func parseRateLimitGroupConfig(jsonStr string) (map[string][2]int, map[string]map[string][2]int, error) {
	raw := make(map[string]any)
	if err := common.UnmarshalJsonStr(jsonStr, &raw); err != nil {
		return nil, nil, err
	}

	simple := make(map[string][2]int)
	byUserToken := make(map[string]map[string][2]int)

	for groupName, groupValue := range raw {
		if limits, err := parseRateLimitPair(groupValue); err == nil {
			simple[groupName] = limits
			continue
		}

		tokenGroupObj, ok := groupValue.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("group %s format invalid, expected [total, success] or object", groupName)
		}

		tokenGroupLimits := make(map[string][2]int)
		for tokenGroup, tokenGroupValue := range tokenGroupObj {
			limits, err := parseRateLimitPair(tokenGroupValue)
			if err != nil {
				return nil, nil, fmt.Errorf("group %s token-group %s format invalid: %w", groupName, tokenGroup, err)
			}
			tokenGroupLimits[tokenGroup] = limits
		}
		byUserToken[groupName] = tokenGroupLimits
	}

	return simple, byUserToken, nil
}

func UpdateModelRequestRateLimitGroupByJSONString(jsonStr string) error {
	simple, byUserToken, err := parseRateLimitGroupConfig(jsonStr)
	if err != nil {
		return err
	}

	ModelRequestRateLimitMutex.Lock()
	defer ModelRequestRateLimitMutex.Unlock()

	ModelRequestRateLimitGroup = simple
	ModelRequestRateLimitByUserTokenGroup = byUserToken
	return nil
}

func GetGroupRateLimit(group string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	if ModelRequestRateLimitGroup == nil {
		return 0, 0, false
	}

	limits, found := ModelRequestRateLimitGroup[group]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func GetGroupRateLimitByUserAndToken(userGroup, tokenGroup string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	if ModelRequestRateLimitByUserTokenGroup == nil || userGroup == "" {
		return 0, 0, false
	}

	limitsByToken, ok := ModelRequestRateLimitByUserTokenGroup[userGroup]
	if !ok {
		return 0, 0, false
	}

	normalizedTokenGroup := tokenGroup
	if normalizedTokenGroup == "" {
		normalizedTokenGroup = userGroup
	}
	limits, found := limitsByToken[normalizedTokenGroup]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func ModelRequestIPRateLimitGroup2JSONString() string {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	jsonBytes, err := common.Marshal(mergeRateLimitGroups(ModelRequestIPRateLimitGroup, ModelRequestIPRateLimitByUserTokenGroup))
	if err != nil {
		common.SysLog("error marshalling model ip group rate limit: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateModelRequestIPRateLimitGroupByJSONString(jsonStr string) error {
	simple, byUserToken, err := parseRateLimitGroupConfig(jsonStr)
	if err != nil {
		return err
	}

	ModelRequestRateLimitMutex.Lock()
	defer ModelRequestRateLimitMutex.Unlock()

	ModelRequestIPRateLimitGroup = simple
	ModelRequestIPRateLimitByUserTokenGroup = byUserToken
	return nil
}

func GetIPGroupRateLimit(group string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	if ModelRequestIPRateLimitGroup == nil {
		return 0, 0, false
	}

	limits, found := ModelRequestIPRateLimitGroup[group]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func GetIPGroupRateLimitByUserAndToken(userGroup, tokenGroup string) (totalCount, successCount int, found bool) {
	ModelRequestRateLimitMutex.RLock()
	defer ModelRequestRateLimitMutex.RUnlock()

	if ModelRequestIPRateLimitByUserTokenGroup == nil || userGroup == "" {
		return 0, 0, false
	}

	limitsByToken, ok := ModelRequestIPRateLimitByUserTokenGroup[userGroup]
	if !ok {
		return 0, 0, false
	}

	normalizedTokenGroup := tokenGroup
	if normalizedTokenGroup == "" {
		normalizedTokenGroup = userGroup
	}
	limits, found := limitsByToken[normalizedTokenGroup]
	if !found {
		return 0, 0, false
	}
	return limits[0], limits[1], true
}

func checkRateLimitGroupMap(rateLimitGroup map[string][2]int) error {
	for group, limits := range rateLimitGroup {
		if limits[0] < 0 || limits[1] < 1 {
			return fmt.Errorf("group %s has negative rate limit values: [%d, %d]", group, limits[0], limits[1])
		}
		if limits[0] > math.MaxInt32 || limits[1] > math.MaxInt32 {
			return fmt.Errorf("group %s [%d, %d] has max rate limits value 2147483647", group, limits[0], limits[1])
		}
	}
	return nil
}

func checkRateLimitNestedGroupMap(rateLimitGroup map[string]map[string][2]int) error {
	for userGroup, tokenGroups := range rateLimitGroup {
		for tokenGroup, limits := range tokenGroups {
			if limits[0] < 0 || limits[1] < 1 {
				return fmt.Errorf("group %s token-group %s has negative rate limit values: [%d, %d]", userGroup, tokenGroup, limits[0], limits[1])
			}
			if limits[0] > math.MaxInt32 || limits[1] > math.MaxInt32 {
				return fmt.Errorf("group %s token-group %s [%d, %d] has max rate limits value 2147483647", userGroup, tokenGroup, limits[0], limits[1])
			}
		}
	}
	return nil
}

func CheckModelRequestRateLimitGroup(jsonStr string) error {
	simple, byUserToken, err := parseRateLimitGroupConfig(jsonStr)
	if err != nil {
		return err
	}
	if err := checkRateLimitGroupMap(simple); err != nil {
		return err
	}
	return checkRateLimitNestedGroupMap(byUserToken)
}

func CheckModelRequestIPRateLimitGroup(jsonStr string) error {
	simple, byUserToken, err := parseRateLimitGroupConfig(jsonStr)
	if err != nil {
		return err
	}
	if err := checkRateLimitGroupMap(simple); err != nil {
		return err
	}
	return checkRateLimitNestedGroupMap(byUserToken)
}
