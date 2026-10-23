//go:build windows

package vmruntime

import "os"

// Keep ordinary Windows API builds available while refusing privileged VM
// helper activation on an unsupported host.
func lockStateFile(*os.File) error { return fault("linux_amd64_required") }
