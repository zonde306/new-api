-- 滑动窗口限流脚本（基于 List）
-- KEYS[1]: 限流器唯一标识
-- ARGV[1]: 最大请求数
-- ARGV[2]: 时间窗口（秒）
-- ARGV[3]: 过期时间（秒）
-- ARGV[4]: 模式（0=仅检查, 1=检查并记录, 2=仅记录, 3=回滚单条记录）
-- ARGV[5]: entry（可选，mode=1/2 用于写入，mode=3 用于回滚）

local key = KEYS[1]
local index_key = key .. ':idx'
local max_requests = tonumber(ARGV[1])
local window_seconds = tonumber(ARGV[2])
local expire_seconds = tonumber(ARGV[3])
local mode = tonumber(ARGV[4])
local custom_entry = ARGV[5]

local function sync_index_ttl()
    if expire_seconds and expire_seconds > 0 then
        redis.call('EXPIRE', index_key, expire_seconds)
    end
end

local function delete_index_by_entry(entry_value)
    local entry_str = tostring(entry_value or '')
    local suffix = string.match(entry_str, '^.+%-(.+)$')
    if suffix and suffix ~= '' then
        redis.call('HDEL', index_key, suffix)
    end
end

if mode == 3 then
    if custom_entry and custom_entry ~= '' then
        -- 优先按完整 entry 精确回滚（兼容未来直接传完整值）
        local removed = redis.call('LREM', key, 1, custom_entry)

        -- 不再做全量 LRANGE 扫描，改为通过索引 O(1) 定位，避免阻塞 Redis 事件循环
        if removed == 0 then
            local indexed_entry = redis.call('HGET', index_key, custom_entry)
            if indexed_entry and indexed_entry ~= '' then
                removed = redis.call('LREM', key, 1, indexed_entry)
                if removed > 0 then
                    delete_index_by_entry(indexed_entry)
                end
            end
        else
            delete_index_by_entry(custom_entry)
        end

        if expire_seconds and expire_seconds > 0 then
            redis.call('EXPIRE', key, expire_seconds)
            redis.call('EXPIRE', index_key, expire_seconds)
        end
        return removed > 0 and 1 or 0
    end
    return 0
end

if not max_requests or max_requests <= 0 then
    return 1
end

if not window_seconds or window_seconds <= 0 then
    return 1
end

local now = redis.call('TIME')
local now_seconds = tonumber(now[1])
local now_micros = tonumber(now[2])
local now_value = string.format('%d.%06d', now_seconds, now_micros)
local now_number = tonumber(now_value)

local entry = now_value
if custom_entry and custom_entry ~= '' then
    entry = now_value .. '-' .. custom_entry
end

local list_len = redis.call('LLEN', key)
local allowed = 1

if list_len >= max_requests then
    local oldest = redis.call('LINDEX', key, -1)
    local oldest_str = tostring(oldest or '')
    -- 兼容 entry 中附带后缀标识（如 "seconds.microseconds-uuid"），仅提取前缀时间用于窗口比较
    local oldest_numeric_str = string.match(oldest_str, '^[0-9]+%.?[0-9]*')
    local oldest_seconds = tonumber(oldest_numeric_str)
    if not oldest_seconds then
        redis.call('DEL', key)
        list_len = 0
    else
        if (now_number - oldest_seconds) < window_seconds then
            allowed = 0
        end
    end
end

if mode == 1 then
    if allowed == 1 then
        redis.call('LPUSH', key, entry)
        if list_len >= max_requests then
            local evicted = redis.call('RPOP', key)
            if evicted then
                delete_index_by_entry(evicted)
            end
        end
        redis.call('LTRIM', key, 0, max_requests - 1)
        if custom_entry and custom_entry ~= '' then
            redis.call('HSET', index_key, custom_entry, entry)
            sync_index_ttl()
        end
        if expire_seconds and expire_seconds > 0 then
            redis.call('EXPIRE', key, expire_seconds)
        end
    else
        if expire_seconds and expire_seconds > 0 then
            redis.call('EXPIRE', key, expire_seconds)
            sync_index_ttl()
        end
    end
elseif mode == 2 then
    redis.call('LPUSH', key, entry)
    if list_len >= max_requests then
        local evicted = redis.call('RPOP', key)
        if evicted then
            delete_index_by_entry(evicted)
        end
    end
    redis.call('LTRIM', key, 0, max_requests - 1)
    if custom_entry and custom_entry ~= '' then
        redis.call('HSET', index_key, custom_entry, entry)
        sync_index_ttl()
    end
    if expire_seconds and expire_seconds > 0 then
        redis.call('EXPIRE', key, expire_seconds)
    end
else
    if allowed == 0 and expire_seconds and expire_seconds > 0 then
        redis.call('EXPIRE', key, expire_seconds)
        sync_index_ttl()
    end
end

return allowed
