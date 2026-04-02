package config

import (
	"fmt"
	"net"
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

	return nil
}
