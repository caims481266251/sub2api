//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Exercise real Redis admission under contention: queued spillover must not
// replace the home binding, exceed the borrowed account limit, or leak slots.
func TestAcquireResponsesSpilloverConcurrentQueueAndCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := testutil.NewRedisGatewayCache(t)
	slots := testutil.NewRedisConcurrencyCache(t)
	concurrency := service.NewConcurrencyService(slots)
	gw := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cache, &config.Config{},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(gw.CloseOpenAIWSPool)
	h := &OpenAIGatewayHandler{gatewayService: gw, concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0)}
	ctx := context.Background()
	groupID := int64(23)
	require.NoError(t, gw.BindStickySession(ctx, &groupID, "contended-session", 1))
	const workers, capacity = 48, 3
	blockers := make([]func(), capacity)
	for i := range blockers {
		slot, err := concurrency.AcquireAccountSlot(ctx, 2, capacity)
		require.NoError(t, err)
		require.True(t, slot.Acquired)
		blockers[i] = slot.ReleaseFunc
		t.Cleanup(slot.ReleaseFunc)
	}
	cancels := make([]context.CancelFunc, workers)
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		requestCtx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		t.Cleanup(cancel)
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(requestCtx)
			selection := &service.AccountSelectionResult{
				Account: profitSlotTestAccount(2, capacity), PreserveStickyBinding: true,
				WaitPlan: &service.AccountWaitPlan{AccountID: 2, MaxConcurrency: capacity, Timeout: 10 * time.Second, MaxWaiting: workers},
			}
			streamStarted := false
			release, result := h.acquireResponsesAccountSlot(c, &groupID, "contended-session", selection, false, &streamStarted, zap.NewNop())
			if index%6 == 0 {
				if release != nil {
					release()
				}
				if result != openAISlotAcquireFailed {
					results <- fmt.Errorf("cancelled worker %d admitted", index)
					return
				}
			} else {
				if result != openAISlotAcquireOK || release == nil {
					results <- fmt.Errorf("worker %d not admitted", index)
					return
				}
				defer release()
				count, err := slots.GetAccountConcurrency(ctx, 2)
				if err != nil || count > capacity {
					results <- fmt.Errorf("account capacity exceeded: count=%d err=%v", count, err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			id, err := cache.GetSessionAccountID(ctx, groupID, "openai:contended-session")
			if err != nil || id != 1 {
				results <- fmt.Errorf("home binding changed: id=%d err=%v", id, err)
				return
			}
			results <- nil
		}(i)
	}
	require.Eventually(t, func() bool {
		count, err := slots.GetAccountWaitingCount(ctx, 2)
		return err == nil && count == workers
	}, 5*time.Second, 10*time.Millisecond)
	for i := 0; i < workers; i += 6 {
		cancels[i]()
	}
	require.Eventually(t, func() bool {
		count, err := slots.GetAccountWaitingCount(ctx, 2)
		return err == nil && count == workers-workers/6
	}, 5*time.Second, 10*time.Millisecond)
	for _, release := range blockers {
		release()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	count, err := slots.GetAccountConcurrency(ctx, 2)
	require.NoError(t, err)
	require.Zero(t, count)
	waiting, err := slots.GetAccountWaitingCount(ctx, 2)
	require.NoError(t, err)
	require.Zero(t, waiting)
	id, err := cache.GetSessionAccountID(ctx, groupID, "openai:contended-session")
	require.NoError(t, err)
	require.Equal(t, int64(1), id)
}

func TestAcquireResponsesAccountSlotPreservesSpilloverBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, wait := range []bool{false, true} {
		for _, preserve := range []bool{false, true} {
			name := "quick"
			if wait {
				name = "queued"
			}
			if preserve {
				name += "/spillover"
			} else {
				name += "/ordinary"
			}
			t.Run(name, func(t *testing.T) {
				cache := testutil.NewRedisGatewayCache(t)
				cfg := &config.Config{}
				gw := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cache, cfg,
					nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				t.Cleanup(gw.CloseOpenAIWSPool)
				sequence := []bool{true}
				if wait {
					sequence = []bool{false, true}
				}
				slots := &helperConcurrencyCacheStub{accountSeq: sequence}
				h := &OpenAIGatewayHandler{gatewayService: gw, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(slots), SSEPingFormatClaude, 0)}
				groupID := int64(23)
				ctx := context.Background()
				require.NoError(t, gw.BindStickySession(ctx, &groupID, "same-conversation", 1))
				selection := &service.AccountSelectionResult{
					Account: profitSlotTestAccount(2, 1), PreserveStickyBinding: preserve,
					WaitPlan: &service.AccountWaitPlan{AccountID: 2, MaxConcurrency: 2, Timeout: time.Second, MaxWaiting: 2},
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				streamStarted := false
				release, result := h.acquireResponsesAccountSlot(c, &groupID, "same-conversation", selection, false, &streamStarted, zap.NewNop())
				require.Equal(t, openAISlotAcquireOK, result)
				require.NotNil(t, release)
				release()
				id, err := cache.GetSessionAccountID(ctx, groupID, "openai:same-conversation")
				require.NoError(t, err)
				want := int64(2)
				if preserve {
					want = 1
				}
				require.Equal(t, want, id)
				require.Equal(t, 1, slots.accountReleaseCalls)
			})
		}
	}
}

func TestOpenAISessionAffinityMessagesTenantIsolation(t *testing.T) {
	contexts := make([]*gin.Context, 2)
	for i := range contexts {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		c.Request.Header.Set("X-Claude-Code-Session-Id", "shared-conversation")
		c.Set("api_key", &service.APIKey{ID: int64(i + 1)})
		contexts[i] = c
	}
	h1, key1 := resolveOpenAIMessagesMetadataSession(contexts[0], "content-hash", "", "gpt-5", nil)
	h2, key2 := resolveOpenAIMessagesMetadataSession(contexts[1], "content-hash", "", "gpt-5", nil)
	require.NotEqual(t, h1, h2)
	require.Empty(t, key1)
	require.Empty(t, key2)
}
