package service

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBorrowSelectionValidationAndDedupe(t *testing.T) {
	s := BorrowSelection{Revision: 2, GroupIDs: []int64{3, 2, 3}, AccountIDs: []int64{11, 11}, DefaultModel: "gpt-6-luna"}
	require.NoError(t, normalizeBorrowSelection(&s))
	require.Equal(t, []int64{2, 3}, s.GroupIDs)
	require.Equal(t, []int64{11}, s.AccountIDs)
	require.NotNil(t, s.ModelOverrides)
	for _, bad := range []BorrowSelection{{Revision: 0, DefaultModel: "gpt-6-luna"}, {Revision: 1, DefaultModel: "gpt-6-luna", AccountIDs: []int64{-1}}, {Revision: 1, DefaultModel: "gpt-6-luna", ModelOverrides: map[int64]string{11: "bad model"}}} {
		require.Error(t, normalizeBorrowSelection(&bad))
	}
}

type normalBorrowTestRepo struct{ *borrowWorkerRepo }

func (r normalBorrowTestRepo) Target(ctx context.Context, id int64) (BorrowTarget, error) {
	if id == 20 {
		return BorrowTarget{AccountID: id}, nil
	}
	return r.borrowWorkerRepo.Target(ctx, id)
}

type normalBorrowSlot struct {
	ConcurrencyCache
	allow              bool
	acquired, released int64
}

func (c *normalBorrowSlot) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	c.acquired = id
	return c.allow, nil
}
func (c *normalBorrowSlot) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.released = id
	return nil
}
func TestBorrowNormalSourceUsesNormalCredentialsAndIndependentConcurrency(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy", true: "available"}[allowed], func(t *testing.T) {
			s, base := borrowFixture()
			a := s.owner.accountRepo.(borrowTestAccounts).account
			a.ID = 20
			a.Concurrency = 2
			a.Credentials["access_token"] = "normal-token"
			a.Credentials["model_mapping"] = map[string]any{"borrow-alias": "gpt-6-luna"}
			r := normalBorrowTestRepo{&borrowWorkerRepo{borrowTestRepo: base, source: BorrowSource{ID: 2, AccountID: 20, Enabled: true, Model: "borrow-alias"}}}
			s.repo = r
			slot := &normalBorrowSlot{allow: allowed}
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo, concurrencyService: &ConcurrencyService{cache: slot}}
			cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
			up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}}}
			s.owner.httpUpstream = up
			_, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
			require.NoError(t, err)
			require.Zero(t, slot.acquired, "source probing must not consume business capacity")
			require.Zero(t, slot.released)
			require.Len(t, up.calls, 1)
			require.Equal(t, "Bearer normal-token", up.calls[0].header.Get("Authorization"))
			require.Contains(t, up.calls[0].body, `"model":"gpt-6-luna"`)
			require.Equal(t, "target", up.calls[0].header.Get("ChatGPT-Account-ID"))
			a.Schedulable = false
			_, err = s.normalSourceAccount(t.Context(), r.source)
			require.Error(t, err)
			a.Schedulable = true
			a.Status = "disabled"
			_, err = s.normalSourceAccount(t.Context(), r.source)
			require.Error(t, err)
		})
	}
}
