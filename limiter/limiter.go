package limiter

// 1. Imports MUST come right after the package name
import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 2. Constants and Globals come AFTER the imports
const TokenBucketLuaScript = `
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local fill_rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local requested = 1

local data = redis.call("HMGET", key, "tokens", "last_updated")
local tokens = tonumber(data[1])
local last_updated = tonumber(data[2])

if tokens == nil then
    tokens = capacity
    last_updated = now
else
    local elapsed = now - last_updated
    if elapsed > 0 then
        tokens = math.min(capacity, tokens + (elapsed * fill_rate))
        last_updated = now
    end
end

if tokens >= requested then
    tokens = tokens - requested
    redis.call("HMSET", key, "tokens", tokens, "last_updated", last_updated)
    redis.call("EXPIRE", key, 3600)
    return {1, math.floor(tokens)}
else
    redis.call("HMSET", key, "tokens", tokens, "last_updated", last_updated)
	return {0, math.floor(tokens)}
end
`

// 3. Types and Structural Methods come last
type RateLimiter struct {
	Client *redis.Client
}

func NewRateLimiter(redisAddr string) *RateLimiter {
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	return &RateLimiter{Client: rdb}
}

func (rl *RateLimiter) Evaluate(ctx context.Context, key string, capacity int, fillRate int) (bool, int, error) {
	now := time.Now().Unix()

	res, err := rl.Client.Eval(ctx, TokenBucketLuaScript, []string{key}, capacity, fillRate, now).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redis script execution failed: %v", err)
	}

	results, ok := res.([]interface{})
	if !ok || len(results) < 2 {
		return false, 0, fmt.Errorf("unexpected script result format")
	}

	allowed := results[0].(int64) == 1
	remaining := int(results[1].(int64))

	return allowed, remaining, nil
}