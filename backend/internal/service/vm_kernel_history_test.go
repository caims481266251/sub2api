package service

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func vmContinuationContext(userID, apiKeyID, groupID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, userID, apiKeyID)
	return c
}

func TestVMKernelContinuationPreservesOwnerRouteAndGeneration(t *testing.T) {
	s := &OpenAIGatewayService{}
	account := &Account{ID: 909091}
	binding := vmkernel.SessionBinding{AccountID: account.ID, InstanceID: "vm-owned", GuestBootID: "boot-owned", Generation: 2}
	ctx := vmkernel.WithSessionBinding(context.Background(), &binding)
	c := vmContinuationContext(10, 20, 30)
	id := "resp_vm_history_owned"
	t.Cleanup(func() {
		vmKernelContinuations.mu.Lock()
		defer vmKernelContinuations.mu.Unlock()
		key := vmKernelContinuationKey{30, 10, account.ID, id}
		entry := vmKernelContinuations.entries[key]
		vmKernelContinuations.bytes -= entry.snapshot.size
		delete(vmKernelContinuations.entries, key)
		vmKernelContinuations.prune(time.Now())
	})
	err := s.rememberVMKernelContinuation(ctx, c, account,
		[]byte(`{"model":"gpt-6-luna","instructions":"old instructions","input":"hello"}`),
		[]byte(`{"id":"resp_vm_history_owned","output":[{"type":"function_call","call_id":"call1","name":"lookup","arguments":"{}"}]}`))
	require.NoError(t, err)
	body := []byte(`{"model":"gpt-6-luna","previous_response_id":"resp_vm_history_owned","instructions":"new instructions","input":[{"type":"function_call_output","call_id":"call1","output":"ok"}]}`)
	// The original owner rule allows another key belonging to the same user.
	fresh, replay, err := s.prepareVMKernelContinuation(context.Background(), vmContinuationContext(10, 21, 30), account, body)
	require.NoError(t, err)
	require.Equal(t, binding, *vmkernel.BindingFromContext(fresh))
	require.Equal(t, 3, len(gjson.GetBytes(replay, "input").Array()))
	require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
	require.Equal(t, "new instructions", gjson.GetBytes(replay, "instructions").String())
	for _, foreign := range []*gin.Context{vmContinuationContext(11, 20, 30), vmContinuationContext(10, 20, 31)} {
		_, _, err := s.prepareVMKernelContinuation(context.Background(), foreign, account, body)
		require.Error(t, err)
	}
	_, _, err = s.prepareVMKernelContinuation(context.Background(), c, &Account{ID: account.ID + 1}, body)
	require.Error(t, err)
	changed := binding
	changed.Generation++
	_, _, err = s.prepareVMKernelContinuation(vmkernel.WithSessionBinding(context.Background(), &changed), c, account, body)
	require.Error(t, err)
	require.Equal(t, uint64(3), changed.Generation, "a live connection binding must not be replaced")
}

func TestVMKernelContinuationCacheBoundedAndExpired(t *testing.T) {
	now := time.Now()
	store := &vmKernelContinuationStore{}
	for i := 0; i < 10; i++ {
		key := vmKernelContinuationKey{responseID: fmt.Sprint(i)}
		require.NoError(t, store.put(key, vmKernelContinuationEntry{snapshot: vmKernelWSSnapshot{size: vmKernelWSHistoryLimit}, expires: now.Add(time.Minute)}, now))
	}
	require.LessOrEqual(t, store.bytes, vmKernelContinuationBytes)
	require.Equal(t, 4, len(store.entries))
	_, found := store.get(vmKernelContinuationKey{responseID: "0"}, now)
	require.False(t, found)
	_, found = store.get(vmKernelContinuationKey{responseID: "9"}, now)
	require.True(t, found)
	_, found = store.get(vmKernelContinuationKey{responseID: "9"}, now.Add(time.Minute))
	require.False(t, found)
	require.Empty(t, store.entries)
	require.Zero(t, store.bytes)
	for i := 0; i < vmKernelContinuationEntries+2; i++ {
		require.NoError(t, store.put(vmKernelContinuationKey{responseID: fmt.Sprint(i)}, vmKernelContinuationEntry{snapshot: vmKernelWSSnapshot{size: 1}, expires: now.Add(time.Minute)}, now))
	}
	require.Len(t, store.entries, vmKernelContinuationEntries)
}

func TestVMKernelContinuationDuplicateNeverRebindsOwnerOrAccount(t *testing.T) {
	s := &OpenAIGatewayService{}
	const group int64 = 998731
	id := "resp_vm_duplicate_immutable"
	original := &Account{ID: 4101}
	owner := vmContinuationContext(71, 81, group)
	binding := vmkernel.SessionBinding{AccountID: original.ID, InstanceID: "original", GuestBootID: "original", Generation: 1}
	response := []byte(`{"id":"resp_vm_duplicate_immutable","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"original"}]}]}`)
	body := []byte(`{"model":"gpt-6-luna","input":"original"}`)
	ctx := vmkernel.WithSessionBinding(context.Background(), &binding)
	require.NoError(t, s.rememberVMKernelContinuation(ctx, owner, original, body, response))
	t.Cleanup(func() {
		vmKernelContinuations.mu.Lock()
		defer vmKernelContinuations.mu.Unlock()
		for key, entry := range vmKernelContinuations.entries {
			if key.groupID == group && key.responseID == id {
				vmKernelContinuations.bytes -= entry.snapshot.size
				delete(vmKernelContinuations.entries, key)
			}
		}
		vmKernelContinuations.prune(time.Now())
	})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, candidate := range []struct{ user, key, account int64 }{{71, 81, 4101}, {72, 82, 4101}, {71, 81, 4102}} {
		wg.Add(1)
		go func(user, key, accountID int64) {
			defer wg.Done()
			other := binding
			other.AccountID = accountID
			errs <- s.rememberVMKernelContinuation(vmkernel.WithSessionBinding(context.Background(), &other), vmContinuationContext(user, key, group), &Account{ID: accountID}, []byte(`{"model":"gpt-6-luna","input":"replacement"}`), response)
		}(candidate.user, candidate.key, candidate.account)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.ErrorContains(t, err, "duplicate response id")
	}
	store := s.getOpenAIWSStateStore()
	accountID, err := store.GetResponseAccount(ctx, group, id)
	require.NoError(t, err)
	require.Equal(t, original.ID, accountID)
	userID, keyID, found, err := store.GetHTTPResponseOwner(ctx, group, id)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 71, userID)
	require.EqualValues(t, 81, keyID)
	_, replay, err := s.prepareVMKernelContinuation(context.Background(), owner, original, []byte(`{"previous_response_id":"resp_vm_duplicate_immutable","input":"next"}`))
	require.NoError(t, err)
	require.Equal(t, "original", gjson.GetBytes(replay, "input.0.content").String())
}

type vmContinuationRedisMissCache struct {
	stubGatewayCache
	readFailure error
}

func (c *vmContinuationRedisMissCache) GetSessionAccountID(_ context.Context, _ int64, key string) (int64, error) {
	if c.readFailure != nil {
		return 0, c.readFailure
	}
	if id, found := c.sessionBindings[key]; found {
		return id, nil
	}
	return 0, ErrStickySessionNotFound
}

func TestVMKernelContinuationPublishesOnRedisMissAndRejectsInfrastructureFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", failure), func(t *testing.T) {
			cache := &vmContinuationRedisMissCache{}
			if failure {
				cache.readFailure = fmt.Errorf("fixture cache unavailable")
			}
			s := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}
			group := int64(910231)
			account := &Account{ID: 910232}
			id := fmt.Sprint("resp_vm_redis_miss_", failure)
			key := vmKernelContinuationKey{group, 7101, account.ID, id}
			t.Cleanup(func() {
				vmKernelContinuations.mu.Lock()
				defer vmKernelContinuations.mu.Unlock()
				if entry, found := vmKernelContinuations.entries[key]; found {
					vmKernelContinuations.bytes -= entry.snapshot.size
					delete(vmKernelContinuations.entries, key)
				}
				vmKernelContinuations.prune(time.Now())
			})
			binding := vmkernel.SessionBinding{AccountID: account.ID, InstanceID: "redis-instance", GuestBootID: "redis-boot", Generation: 14}
			ctx := vmkernel.WithSessionBinding(context.Background(), &binding)
			owner := vmContinuationContext(7101, 7102, group)
			response := []byte(fmt.Sprintf(`{"id":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`, id))
			err := s.rememberVMKernelContinuation(ctx, owner, account, []byte(`{"model":"gpt-6-luna","input":"Remember marker"}`), response)
			if failure {
				require.ErrorContains(t, err, "cache unavailable")
				require.Empty(t, cache.sessionBindings, "cache failure must not publish ownership")
				return
			}
			require.NoError(t, err)
			owned, err := s.ValidateOpenAIHTTPResponseOwner(context.Background(), group, id, 7101, 7102)
			require.NoError(t, err)
			require.True(t, owned, "the public HTTP owner precheck must find the completed seed")
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-luna","previous_response_id":%q,"input":"Recall marker"}`, id))
			fresh, replay, err := s.prepareVMKernelContinuation(context.Background(), vmContinuationContext(7101, 7102, group), account, body)
			require.NoError(t, err, "a fresh HTTP or WS context must replay Redis-backed ownership")
			require.Equal(t, binding, *vmkernel.BindingFromContext(fresh))
			require.Len(t, gjson.GetBytes(replay, "input").Array(), 3)
			require.ErrorContains(t, s.rememberVMKernelContinuation(ctx, owner, account, []byte(`{"input":"replacement"}`), response), "duplicate response id")
		})
	}
}
