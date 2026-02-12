-- 滑动窗口限流脚本（基于 List）
-- KEYS[1]: 限流器唯一标识
-- ARGV[1]: 最大请求数
-- ARGV[2]: 时间窗口（秒）
-- ARGV[3]: 过期时间（秒）
-- ARGV[4]: 模式（0=仅检查, 1=检查并记录, 2=仅记录）

local key = KEYS[1]
local max_requests = tonumber(ARGV[1])
local window_seconds = tonumber(ARGV[2])
local expire_seconds = tonumber(ARGV[3])
local mode = tonumber(ARGV[4])

if not max_requests or max_requests <= 0 then
    return 1
end

if not window_seconds or window_seconds <= 0 then
    return 1
end

local now = redis.call('TIME')
local now_seconds = tonumber(now[1])

local list_len = redis.call('LLEN', key)
local allowed = 1

if list_len >= max_requests then
    local oldest = redis.call('LINDEX', key, -1)
    local oldest_seconds = tonumber(oldest)
    if not oldest_seconds then
        redis.call('DEL', key)
        list_len = 0
    else
        if (now_seconds - oldest_seconds) < window_seconds then
            allowed = 0
        end
    end
end

if mode == 1 then
    if allowed == 1 then
        redis.call('LPUSH', key, now_seconds)
        redis.call('LTRIM', key, 0, max_requests - 1)
        if expire_seconds and expire_seconds > 0 then
            redis.call('EXPIRE', key, expire_seconds)
        end
    else
        if expire_seconds and expire_seconds > 0 then
            redis.call('EXPIRE', key, expire_seconds)
        end
    end
elseif mode == 2 then
    redis.call('LPUSH', key, now_seconds)
    redis.call('LTRIM', key, 0, max_requests - 1)
    if expire_seconds and expire_seconds > 0 then
        redis.call('EXPIRE', key, expire_seconds)
    end
else
    if allowed == 0 and expire_seconds and expire_seconds > 0 then
        redis.call('EXPIRE', key, expire_seconds)
    end
end

return allowed
