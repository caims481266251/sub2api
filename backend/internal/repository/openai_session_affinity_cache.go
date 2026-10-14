package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var _ service.OpenAISessionAffinityCache = (*gatewayCache)(nil)

func openAISessionAffinityKeys(key string) []string {
	// Both keys share one Redis Cluster slot. This namespace never contains
	// response ownership, credentials, history or legacy sticky records.
	base := "openai_session_affinity:v1:{" + key + "}:"
	return []string{base + "owner", base + "pending"}
}

var readOpenAISessionAffinity = redis.NewScript(`
return redis.call('GET',KEYS[1]) or redis.call('GET',KEYS[2])
`)

var claimOpenAISessionAffinity = redis.NewScript(`
local current = redis.call('GET',KEYS[1]) or redis.call('GET',KEYS[2])
if current then return current end
redis.call('SET',KEYS[2],ARGV[1],'PX',ARGV[2])
return ARGV[1]
`)

var completeOpenAISessionAffinity = redis.NewScript(`
local expected = cjson.decode(ARGV[1])
local next = cjson.decode(ARGV[2])
local raw = redis.call('GET',KEYS[1])
if raw then
  local current = cjson.decode(raw)
  if current.version ~= expected.version then return 0 end
  -- Another request confirmed this provisional generation. Only its owner
  -- may refresh it; an older provisional spillover cannot take it back.
  if expected.pending and current.account_id ~= next.account_id then return 0 end
else
  if not expected.pending then return 0 end
  raw = redis.call('GET',KEYS[2])
  if not raw or cjson.decode(raw).version ~= expected.version then return 0 end
end
redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3])
redis.call('DEL',KEYS[2])
return 1
`)

func decodeOpenAISessionAffinity(raw string, err error) (*service.OpenAISessionAffinityBinding, error) {
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b service.OpenAISessionAffinityBinding
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, err
	}
	if b.AccountID <= 0 || b.Version == "" || (b.Channel != "ordinary" && b.Channel != "borrowed") {
		return nil, errors.New("invalid OpenAI session affinity record")
	}
	return &b, nil
}

func (c *gatewayCache) GetOpenAISessionAffinity(ctx context.Context, key string) (*service.OpenAISessionAffinityBinding, error) {
	raw, err := readOpenAISessionAffinity.Run(ctx, c.rdb, openAISessionAffinityKeys(key)).Text()
	return decodeOpenAISessionAffinity(raw, err)
}

func (c *gatewayCache) ClaimOpenAISessionAffinity(ctx context.Context, key string, next service.OpenAISessionAffinityBinding, ttl time.Duration) (*service.OpenAISessionAffinityBinding, error) {
	if ttl <= 0 || next.AccountID <= 0 || next.Version == "" {
		return nil, errors.New("invalid OpenAI session affinity claim")
	}
	next.Pending = true
	data, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	raw, err := claimOpenAISessionAffinity.Run(ctx, c.rdb, openAISessionAffinityKeys(key), string(data), max(1, ttl.Milliseconds())).Text()
	return decodeOpenAISessionAffinity(raw, err)
}

func (c *gatewayCache) CompleteOpenAISessionAffinity(ctx context.Context, key string, expected, next service.OpenAISessionAffinityBinding, ttl time.Duration) (bool, error) {
	if ttl <= 0 || expected.Version == "" || next.AccountID <= 0 || next.Version == "" {
		return false, errors.New("invalid OpenAI session affinity completion")
	}
	next.Pending = false
	expectData, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	nextData, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	committed, err := completeOpenAISessionAffinity.Run(ctx, c.rdb, openAISessionAffinityKeys(key), string(expectData), string(nextData), max(1, ttl.Milliseconds())).Int()
	return committed == 1, err
}
