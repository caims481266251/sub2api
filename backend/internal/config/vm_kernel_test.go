package config

import "testing"

func TestVMKernelConfigEnvironmentAndDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("VM_KERNEL_ENABLED", "true")
	t.Setenv("VM_KERNEL_HELPER_SOCKET", "/run/sub2api-vm/helper.sock")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VMKernel.Enabled || cfg.VMKernel.HelperSocket != "/run/sub2api-vm/helper.sock" || cfg.VMKernel.Template != "default" || cfg.VMKernel.CPU != 2 || cfg.VMKernel.MemoryMiB != 512 {
		t.Fatalf("VM environment/default mismatch: %+v", cfg.VMKernel)
	}
}

func TestVMKernelConfigValidation(t *testing.T) {
	valid := Config{VMKernel: VMKernelConfig{Enabled: true, HelperSocket: "/run/vm/helper.sock", Template: "default", CPU: 2, MemoryMiB: 2048}}
	if err := valid.validateVMKernel(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"relative socket":      func(c *Config) { c.VMKernel.HelperSocket = "relative.sock" },
		"template path":        func(c *Config) { c.VMKernel.Template = "../template" },
		"cpu":                  func(c *Config) { c.VMKernel.CPU = 33 },
		"memory":               func(c *Config) { c.VMKernel.MemoryMiB = 127 },
		"gateway":              func(c *Config) { c.Runtime.Role = RuntimeRoleGateway },
		"regional scheduler":   func(c *Config) { c.Runtime.RegionalSchedulerFile = "snapshot" },
		"regional auth":        func(c *Config) { c.Runtime.RegionalAuthFile = "auth" },
		"regional concurrency": func(c *Config) { c.Runtime.RegionalConcurrencyFile = "concurrency" },
		"regional funds":       func(c *Config) { c.Runtime.RegionalFundsFile = "funds" },
		"serverless":           func(c *Config) { c.Runtime.ServerlessID = "replica" },
		"upstream routing":     func(c *Config) { c.Gateway.UpstreamRouting.Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			change(&c)
			if c.validateVMKernel() == nil {
				t.Fatal("invalid enabled VM config accepted")
			}
			c.VMKernel.Enabled = false
			if err := c.validateVMKernel(); err != nil {
				t.Fatal("disabled VM affected ordinary deployment", err)
			}
		})
	}
}
