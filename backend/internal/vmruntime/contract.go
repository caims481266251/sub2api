// Package vmruntime is the local, account-scoped KVM helper protocol. It does
// not select accounts, refresh OAuth credentials, or record billable usage.
package vmruntime

import (
	"context"
	"encoding/json"
	"time"
)

const ProtocolVersion = 1

type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Spec contains no command, mount, host path, or arbitrary image override.
// Template selects a helper-operator configured immutable deployment template.
type Spec struct {
	Platform      string `json:"platform"`
	Template      string `json:"template"`
	CPU           int    `json:"cpu"`
	MemoryMiB     int    `json:"memory_mib"`
	DiskGiB       int    `json:"disk_gib"`
	ProxyID       int64  `json:"proxy_id"`
	ProxyRevision string `json:"proxy_revision"`
}

// Projection is already converted to the fixed kernel's credential schema by
// the authoritative account service. Neither content nor its digest is public.
type Projection struct {
	Revision   string          `json:"revision"`
	ProxyURL   string          `json:"proxy_url,omitempty"`
	Credential json.RawMessage `json:"credential"`
}

type Request struct {
	// MigrateNetwork is a private operator action after the application is drained.
	MigrateNetwork bool        `json:"migrate_network,omitempty"`
	Version        int         `json:"version"`
	Operation      string      `json:"operation"`
	AccountID      int64       `json:"account_id"`
	InstanceID     string      `json:"instance_id"`
	Generation     uint64      `json:"generation"`
	Spec           *Spec       `json:"spec,omitempty"`
	Projection     *Projection `json:"projection,omitempty"`
}

type Identity struct {
	UUID       string `json:"uuid"`
	Serial     string `json:"serial"`
	MAC        string `json:"mac"`
	DiskSerial string `json:"disk_serial"`
	MachineID  string `json:"machine_id"`
	Hostname   string `json:"hostname"`
}

// ClientIdentity is the stable per-instance kernel client identity. It is never
// accepted from account input and remains unchanged across credential refresh.
type ClientIdentity struct {
	DeviceID string `json:"device_id"`
}

// NetworkLease is allocated by the helper, never accepted from account input.
// Empty Network on an existing manifest denotes its unchanged legacy network.
type NetworkLease struct {
	NetworkName  string `json:"network_name"`
	Subnet       string `json:"subnet"`
	Gateway      string `json:"gateway"`
	Address      string `json:"address"`
	GuestSubnet  string `json:"guest_subnet"`
	GuestGateway string `json:"guest_gateway"`
	GuestAddress string `json:"guest_address"`
	GuestDNS     string `json:"guest_dns"`
	Released     bool   `json:"released,omitempty"`
}

// State is a private control-plane DTO, never an account API response. The
// service must gate scheduling on Ready AND the currently expected revisions.
type State struct {
	ClientIdentity            *ClientIdentity `json:"client_identity,omitempty"`
	Network                   *NetworkLease   `json:"network,omitempty"`
	AccountID                 int64           `json:"account_id"`
	InstanceID                string          `json:"instance_id"`
	Generation                uint64          `json:"generation"`
	Phase                     string          `json:"phase"`
	Ready                     bool            `json:"ready"`
	ActualAccelerator         string          `json:"actual_accelerator,omitempty"`
	Capacity                  int             `json:"capacity"` // 0 means account-scheduler managed; legacy positive values are informational
	Spec                      Spec            `json:"spec"`
	Identity                  Identity        `json:"identity"`
	ContainerID               string          `json:"container_id,omitempty"`
	GuestBootID               string          `json:"guest_boot_id,omitempty"`
	DesiredCredentialRevision string          `json:"desired_credential_revision,omitempty"`
	AppliedCredentialRevision string          `json:"applied_credential_revision,omitempty"`
	AppliedProxyRevision      string          `json:"applied_proxy_revision,omitempty"`
	KernelSocket              string          `json:"kernel_socket,omitempty"`
	InternalToken             string          `json:"internal_token,omitempty"` // private UDS response only; never publish/log
	DeleteRequested           bool            `json:"delete_requested"`
	DataRetained              bool            `json:"data_retained"`
	ErrorCode                 string          `json:"error_code,omitempty"`
	UpdatedAt                 time.Time       `json:"updated_at"`
}

type Error struct {
	Code string `json:"code"`
}

func (e *Error) Error() string { return e.Code }
func fault(code string) error  { return &Error{Code: code} }

// Driver is deliberately account-scoped. All filesystem paths are derived by
// the helper, not supplied over its API. A timeout is an unknown outcome and
// the helper retains the resource identity for subsequent inspection.
type Driver interface {
	Preflight(context.Context, State) error
	Ensure(context.Context, State, string) (string, error)
	Inspect(context.Context, State, string) (Observation, error)
	Apply(context.Context, State, string) (Observation, error)
	Stop(context.Context, State) error
	Remove(context.Context, State) error
}

type Observation struct {
	ActualAccelerator  string
	Running            bool
	KVMEnabled         bool
	GuestBootID        string
	CredentialRevision string
	ProxyRevision      string
	KernelReady        bool
}
