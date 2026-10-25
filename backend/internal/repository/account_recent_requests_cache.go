package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type accountRecentRequestsCache struct {
	client *redis.Client
	writes chan struct{}
}

func NewAccountRecentRequestsCache(shared *redis.Client) service.AccountRecentRequestsCache {
	if shared == nil {
		return nil
	}
	opts := *shared.Options()
	opts.ContextTimeoutEnabled = true
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout, opts.PoolTimeout = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	opts.MaxRetries, opts.PoolSize, opts.MinIdleConns = -1, 4, 0
	return &accountRecentRequestsCache{client: redis.NewClient(&opts), writes: make(chan struct{}, 4)}
}

func accountRecentRequestsKey(id int64, scope string) string {
	return fmt.Sprintf("account:recent_requests:%d:%s", id, scope)
}

const appendAccountRecentRequest = `
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -17)
redis.call('EXPIRE', KEYS[1], 604800)
return 1
`

func (c *accountRecentRequestsCache) Append(ctx context.Context, scope string, record service.AccountRecentRequest) error {
	select {
	case c.writes <- struct{}{}:
		defer func() { <-c.writes }()
	default:
		return nil
	}
	if scope == "" || record.AccountID <= 0 {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Eval avoids the extra NOSCRIPT round trip on the first observed request.
	return c.client.Eval(ctx, appendAccountRecentRequest, []string{accountRecentRequestsKey(record.AccountID, scope)}, record.CompletedAt.UnixMicro(), string(data)).Err()
}

func (c *accountRecentRequestsCache) Batch(ctx context.Context, scopes map[int64]string) (map[int64][]service.AccountRecentRequest, error) {
	pipe := c.client.Pipeline()
	cmds := make(map[int64]*redis.StringSliceCmd, len(scopes))
	for id, scope := range scopes {
		cmds[id] = pipe.ZRange(ctx, accountRecentRequestsKey(id, scope), 0, -1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	result := make(map[int64][]service.AccountRecentRequest, len(scopes))
	for id, cmd := range cmds {
		rows, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		records := make([]service.AccountRecentRequest, 0, len(rows))
		for _, row := range rows {
			var record service.AccountRecentRequest
			if json.Unmarshal([]byte(row), &record) == nil && record.AccountID == id {
				records = append(records, record)
			}
		}
		result[id] = records
	}
	return result, nil
}
func (c *accountRecentRequestsCache) Close() error { return c.client.Close() }
