package service

// VMKernelSchedulerProjection seals the revision of an authoritative full
// account before secrets and proxy fields are removed from scheduler metadata.
// Already-projected input is copied, never hashed as if it were authoritative.
func VMKernelSchedulerProjection(a *Account) *VMKernelSchedulerRevision {
	if a == nil || !a.IsVMKernel() {
		return nil
	}
	if a.VMKernelSchedulerRevision != nil {
		copy := *a.VMKernelSchedulerRevision
		return &copy
	}
	if a.GetCredential("access_token") == "" || a.Proxy == nil || a.ProxyID == nil {
		return nil
	}
	return &VMKernelSchedulerRevision{Credential: vmCredentialRevision(a), Proxy: vmProxyRevision(a)}
}
