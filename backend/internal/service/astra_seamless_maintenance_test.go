package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type seamlessMaintenanceStub struct {
	*astraSetupUpstream
	calls   chan int64
	stopped atomic.Int32
}

func (s *seamlessMaintenanceStub) MaintainAstraGatewayTarget(_ context.Context, _ *http.Request, _ string, id int64, _ int) {
	select {
	case s.calls <- id:
	default:
	}
}
func (s *seamlessMaintenanceStub) BlockAstraGatewayTarget(context.Context, int64, string) {}
func (s *seamlessMaintenanceStub) StopAstraSeamless()                                     { s.stopped.Add(1) }
func TestSeamlessMaintenanceStartsWithoutSettingsSave(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = config.CodexGatewayPinConfig{Enabled: true, Seamless: true, SourceAccountIDs: []int64{8}, TargetAccountIDs: []int64{7}}
	account := stateProbeAccount()
	account.Status = StatusActive
	account.Schedulable = false
	provider := &seamlessMaintenanceStub{astraSetupUpstream: &astraSetupUpstream{}, calls: make(chan int64, 1)}
	svc := &AccountTestService{cfg: cfg, accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: provider, openaiGatewayService: &OpenAIGatewayService{}}
	stop := svc.startAstraSeamlessMaintenance()
	select {
	case id := <-provider.calls:
		require.EqualValues(t, 7, id)
	case <-time.After(time.Second):
		stop()
		t.Fatal("startup did not maintain saved enabled configuration")
	}
	stop()
	require.EqualValues(t, 1, provider.stopped.Load())
}
func TestSeamlessTemplateRejectsExpiredAccount(t *testing.T) {
	account := stateProbeAccount()
	account.Status = StatusActive
	expired := time.Now().Add(-time.Second)
	account.ExpiresAt = &expired
	svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, openaiGatewayService: &OpenAIGatewayService{}}
	_, _, _, err := svc.astraTargetTemplate(t.Context(), 7)
	require.ErrorContains(t, err, "account_unavailable")
}
