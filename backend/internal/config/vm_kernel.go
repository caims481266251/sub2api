package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// VMKernelConfig is deployment configuration, not an account-supplied command
// or filesystem location. The zero value keeps ordinary deployments unchanged.
type VMKernelConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	HelperSocket string `mapstructure:"helper_socket"`
	Template     string `mapstructure:"template"`
	CPU          int    `mapstructure:"cpu"`
	MemoryMiB    int    `mapstructure:"memory_mib"`
	DiskGiB      int    `mapstructure:"disk_gib"`
}

func (c *Config) validateVMKernel() error {
	if !c.VMKernel.Enabled {
		return nil
	}
	vm := c.VMKernel
	if !filepath.IsAbs(vm.HelperSocket) || strings.TrimSpace(vm.HelperSocket) != vm.HelperSocket || strings.ContainsRune(vm.HelperSocket, '\x00') {
		return fmt.Errorf("vm_kernel.helper_socket must be an absolute socket path")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`).MatchString(vm.Template) {
		return fmt.Errorf("vm_kernel.template must be a named deployment template")
	}
	if vm.CPU < 1 || vm.CPU > 32 {
		return fmt.Errorf("vm_kernel.cpu must be between 1 and 32")
	}
	if vm.MemoryMiB < 512 || vm.MemoryMiB > 131072 {
		return fmt.Errorf("vm_kernel.memory_mib must be between 512 and 131072")
	}
	if vm.DiskGiB < 4 || vm.DiskGiB > 1024 {
		return fmt.Errorf("vm_kernel.disk_gib must be between 4 and 1024")
	}
	// Guest lifetime and accounting require the single local coordinator. Regional
	// snapshots and funds/concurrency files belong to separate execution domains.
	r := c.Runtime
	if (r.Role != "" && r.Role != RuntimeRoleFull) || r.RegionalSchedulerFile != "" || r.RegionalAuthFile != "" || r.RegionalConcurrencyFile != "" || r.RegionalFundsFile != "" || r.ServerlessID != "" || r.ServerlessEndpoint != "" || r.ServerlessRegion != "" || r.ServerlessSecret != "" || c.Gateway.UpstreamRouting.Enabled {
		return fmt.Errorf("vm_kernel requires a single local full runtime without regional, serverless, or upstream routing")
	}
	return nil
}
