package service

import "testing"

func TestVMKernelProxyAddressChangeDrainsWithoutRotatingIdentity(t *testing.T) {
	s, a, f := vmLifecycleSetup(t)
	s.prepare(t.Context(), a)
	identity := *f.state.ClientIdentity
	credential, proxy := f.state.AppliedCredentialRevision, f.state.AppliedProxyRevision
	f.state.ProxyAddressChanged = true
	slot := s.slot(a.ID)
	slot.active = 2
	s.prepare(t.Context(), a)
	if f.state.Generation != 1 || VMKernelRuntimeReady(a) || f.kernelCalls != 0 {
		t.Fatal("DNS refresh interrupted active requests or admitted new work")
	}
	slot.active = 0
	s.prepare(t.Context(), a)
	if f.state.Generation != 2 || f.state.ProxyAddressChanged || !VMKernelRuntimeReady(a) ||
		*f.state.ClientIdentity != identity || f.state.AppliedCredentialRevision != credential || f.state.AppliedProxyRevision != proxy {
		t.Fatal("DNS refresh did not preserve account identity and proxy binding")
	}
	s.prepare(t.Context(), a)
	if f.state.Generation != 2 {
		t.Fatal("DNS refresh repeated after application")
	}
}
