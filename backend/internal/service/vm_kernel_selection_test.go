package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func TestVMKernelSelectionReadinessAndManualDisable(t *testing.T) {
	account := &Account{ID: 987654321, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "test-token"}}
	vmKernelReadiness.Delete(account.ID)
	t.Cleanup(func() { vmKernelReadiness.Delete(account.ID) })
	if account.IsSchedulable() {
		t.Fatal("unprepared VM scheduled")
	}
	ready := vmRuntimeStatus{state: vmruntime.State{Ready: true}, credentials: vmCredentialRevision(account), proxy: vmProxyRevision(account), checked: time.Now()}
	vmKernelReadiness.Store(account.ID, ready)
	if !account.IsSchedulable() {
		t.Fatal("ready VM excluded")
	}
	account.Schedulable = false
	if account.IsSchedulable() {
		t.Fatal("readiness bypassed manual disable")
	}
	account.Schedulable = true
	account.Credentials["access_token"] = "rotated-token"
	if account.IsSchedulable() {
		t.Fatal("stale credential projection scheduled")
	}
	account.Credentials["access_token"] = "test-token"
	if account.IsSchedulableForModelWithContext(WithVMKernelExcluded(context.Background()), "gpt-6-sol") {
		t.Fatal("unsupported endpoint selected VM")
	}
	ready.checked = time.Now().Add(-time.Minute)
	vmKernelReadiness.Store(account.ID, ready)
	if account.IsSchedulable() {
		t.Fatal("stale runtime observation scheduled")
	}
	account.ExecutionMode = AccountExecutionModeNormal
	if !account.IsSchedulable() {
		t.Fatal("normal account changed by VM readiness")
	}
}

func TestVMKernelSelectionTransportAndCapabilities(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
	svc := &OpenAIGatewayService{}
	for _, transport := range []OpenAIUpstreamTransport{OpenAIUpstreamTransportAny, OpenAIUpstreamTransportHTTPSSE, OpenAIUpstreamTransportResponsesWebsocketV2, OpenAIUpstreamTransportResponsesWebsocketV2Ingress} {
		if !svc.isOpenAIAccountTransportCompatible(account, transport) {
			t.Fatalf("VM transport rejected: %s", transport)
		}
	}
	for _, capability := range []OpenAIEndpointCapability{OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityResponses} {
		if !account.SupportsOpenAIEndpointCapability(capability) {
			t.Fatalf("text capability rejected: %s", capability)
		}
	}
	for _, capability := range []OpenAIEndpointCapability{OpenAIEndpointCapabilityResponsesCompact, OpenAIEndpointCapabilityAlphaSearch, OpenAIEndpointCapabilityEmbeddings, OpenAIEndpointCapabilityLive} {
		if account.SupportsOpenAIEndpointCapability(capability) {
			t.Fatalf("unsupported capability accepted: %s", capability)
		}
	}
	if account.AllowsOpenAICompact() || openAICompactSupportTier(account) != 0 || account.SupportsOpenAIImageCapability(OpenAIImagesCapabilityBasic) {
		t.Fatal("VM accepted compact or images")
	}
	if isOpenAICodexTicketAccount(account) || !openAICodexSkipHarvest(account) {
		t.Fatal("VM entered host ticket flow")
	}
	if openAIImageRequestRejection(WithOpenAIImagesEndpoint(context.Background()), account, "gpt-image-2") == "" {
		t.Fatal("VM accepted image request")
	}
}

func TestVMKernelSchedulerMetadataRevisionFences(t *testing.T) {
	proxyID := int64(765432)
	a := &Account{ID: 765432, ExecutionMode: AccountExecutionModeVMKernel, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "fixture-original"}, ProxyID: &proxyID,
		Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "fixture.invalid", Port: 8080}}
	publish := func() {
		vmKernelReadiness.Store(a.ID, vmRuntimeStatus{state: vmruntime.State{Ready: true}, checked: time.Now(), credentials: vmCredentialRevision(a), proxy: vmProxyRevision(a)})
	}
	t.Cleanup(func() { vmKernelReadiness.Delete(a.ID) })
	publish()
	metadata := *a
	metadata.VMKernelSchedulerRevision = VMKernelSchedulerProjection(a)
	metadata.Credentials, metadata.Proxy, metadata.ProxyID = nil, nil, nil
	if !metadata.IsSchedulable() {
		t.Fatal("secret-free scheduler projection lost valid readiness")
	}
	a.Credentials["access_token"] = "fixture-rotated"
	publish()
	if metadata.IsSchedulable() {
		t.Fatal("old metadata survived credential revision change")
	}
	metadata.VMKernelSchedulerRevision = VMKernelSchedulerProjection(a)
	if !metadata.IsSchedulable() {
		t.Fatal("refreshed credential projection not schedulable")
	}
	a.Proxy.Host = "changed.invalid"
	publish()
	if metadata.IsSchedulable() {
		t.Fatal("old metadata survived proxy revision change")
	}
	metadata.VMKernelSchedulerRevision = nil
	if metadata.IsSchedulable() {
		t.Fatal("legacy metadata bypassed revisions")
	}
	a.VMKernelSchedulerRevision = VMKernelSchedulerProjection(a)
	if a.IsSchedulable() {
		t.Fatal("full account accepted metadata override")
	}
}
