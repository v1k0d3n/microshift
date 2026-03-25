package config

import (
	"fmt"
	"net"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TwoNodeRole defines the role of a node in a 2-node HA cluster.
// +kubebuilder:validation:Enum:="";primary;secondary
type TwoNodeRole string

const (
	// TwoNodeRoleUnset indicates the role has not been assigned.
	TwoNodeRoleUnset TwoNodeRole = ""
	// TwoNodeRolePrimary indicates this node initializes the cluster.
	TwoNodeRolePrimary TwoNodeRole = "primary"
	// TwoNodeRoleSecondary indicates this node joins an existing cluster.
	TwoNodeRoleSecondary TwoNodeRole = "secondary"
)

// TwoNodeConfig holds configuration for 2-node HA mode. When enabled,
// MicroShift runs with a Kine storage backend backed by PostgreSQL with
// synchronous streaming replication, and both nodes run the full control
// plane with leader-elected components.
type TwoNodeConfig struct {
	// Enabled activates 2-node HA mode. When false (default), MicroShift
	// runs in single-node mode with the embedded etcd backend.
	// +kubebuilder:validation:Optional
	Enabled bool `json:"enabled"`

	// Role indicates whether this node is the "primary" (initializer)
	// or "secondary" (joiner) in the 2-node cluster. This is typically
	// set during init-cluster or join-cluster and should not be changed
	// manually after cluster initialization.
	// +kubebuilder:validation:Optional
	Role TwoNodeRole `json:"role,omitempty"`

	// VIP is the Virtual IP address shared between both nodes. Clients
	// (including kubectl) use this address to reach the Kubernetes API
	// server. The VIP floats to whichever node is currently serving as
	// the active API endpoint.
	// +kubebuilder:validation:Optional
	VIP string `json:"vip,omitempty"`

	// VIPInterface is the network interface on which to bind the VIP.
	// If empty, it is auto-detected from the interface that owns the
	// node's primary IP address.
	// +kubebuilder:validation:Optional
	VIPInterface string `json:"vipInterface,omitempty"`

	// Peer holds connection information for the other node in the
	// 2-node cluster.
	// +kubebuilder:validation:Optional
	Peer TwoNodePeer `json:"peer,omitempty"`

	// Failover controls automatic PostgreSQL failover behavior when the
	// peer node becomes unreachable. When enabled, a watchdog monitors
	// the peer's Patroni health and triggers promotion if the local node
	// is a PG replica and the peer is confirmed down.
	// +kubebuilder:validation:Optional
	Failover FailoverConfig `json:"failover,omitempty"`
}

// FailoverConfig controls the automatic failover watchdog that promotes the
// local PostgreSQL replica when the peer node is unreachable.
type FailoverConfig struct {
	// Enabled activates the failover watchdog. Defaults to true when
	// twoNode.enabled is true.
	// +kubebuilder:validation:Optional
	Enabled *bool `json:"enabled,omitempty"`

	// FailureThreshold is the number of consecutive health check failures
	// before triggering failover. Default: 6 (with 5s interval = 30s dampening).
	// +kubebuilder:validation:Optional
	FailureThreshold int `json:"failureThreshold,omitempty"`

	// CheckInterval is the time between peer health checks. Default: 5s.
	// +kubebuilder:validation:Optional
	CheckInterval *metav1.Duration `json:"checkInterval,omitempty"`
}

// TwoNodePeer describes the other node in a 2-node HA cluster.
type TwoNodePeer struct {
	// Address is the IP address of the peer node.
	// +kubebuilder:validation:Optional
	Address string `json:"address,omitempty"`

	// Hostname is the hostname of the peer node. If empty, it will be
	// resolved from the address during cluster initialization.
	// +kubebuilder:validation:Optional
	Hostname string `json:"hostname,omitempty"`
}

const (
	// DefaultFailoverFailureThreshold is the number of consecutive health
	// check failures (one per MicroShift restart) before triggering a
	// failover. Set to 5 to stay within systemd's default StartLimitBurst
	// of 5 restarts per 10 seconds.
	DefaultFailoverFailureThreshold = 5

	// DefaultFailoverCheckInterval is the time between peer health checks.
	DefaultFailoverCheckInterval = 5 * time.Second
)

// FailoverDefaults returns a FailoverConfig with default values.
func FailoverDefaults() FailoverConfig {
	enabled := true
	return FailoverConfig{
		Enabled:          &enabled,
		FailureThreshold: DefaultFailoverFailureThreshold,
		CheckInterval:    &metav1.Duration{Duration: DefaultFailoverCheckInterval},
	}
}

// IsEnabled returns whether the failover watchdog is enabled. Defaults to
// true when the pointer is nil (i.e., user didn't explicitly set it).
func (f *FailoverConfig) IsEnabled() bool {
	if f.Enabled == nil {
		return true
	}
	return *f.Enabled
}

// EffectiveCheckInterval returns the check interval with the default applied.
func (f *FailoverConfig) EffectiveCheckInterval() time.Duration {
	if f.CheckInterval == nil || f.CheckInterval.Duration == 0 {
		return DefaultFailoverCheckInterval
	}
	return f.CheckInterval.Duration
}

// EffectiveFailureThreshold returns the failure threshold with the default applied.
func (f *FailoverConfig) EffectiveFailureThreshold() int {
	if f.FailureThreshold <= 0 {
		return DefaultFailoverFailureThreshold
	}
	return f.FailureThreshold
}

// validate checks TwoNodeConfig for consistency. It is called as part of
// the top-level Config.validate().
func (t *TwoNodeConfig) validate() error {
	if !t.Enabled {
		return nil
	}

	if t.VIP == "" {
		return fmt.Errorf("twoNode.vip is required when twoNode.enabled is true")
	}
	if net.ParseIP(t.VIP) == nil {
		return fmt.Errorf("twoNode.vip %q is not a valid IP address", t.VIP)
	}

	if t.Peer.Address == "" {
		return fmt.Errorf("twoNode.peer.address is required when twoNode.enabled is true")
	}
	if net.ParseIP(t.Peer.Address) == nil {
		return fmt.Errorf("twoNode.peer.address %q is not a valid IP address", t.Peer.Address)
	}

	if t.Role != TwoNodeRoleUnset && t.Role != TwoNodeRolePrimary && t.Role != TwoNodeRoleSecondary {
		return fmt.Errorf("twoNode.role must be %q or %q, got %q", TwoNodeRolePrimary, TwoNodeRoleSecondary, t.Role)
	}

	if t.Failover.FailureThreshold < 0 {
		return fmt.Errorf("twoNode.failover.failureThreshold must be >= 0, got %d", t.Failover.FailureThreshold)
	}
	if t.Failover.CheckInterval != nil && t.Failover.CheckInterval.Duration < time.Second {
		return fmt.Errorf("twoNode.failover.checkInterval must be >= 1s, got %v", t.Failover.CheckInterval.Duration)
	}

	return nil
}
