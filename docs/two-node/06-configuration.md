# Phase 6: Configuration Schema Changes

## Current Configuration
MicroShift uses `/etc/microshift/config.yaml` with a Go struct-based schema.
The config is loaded in `pkg/config/config.go`.

## New Configuration Additions

### Top-level `twoNode` section
```yaml
# /etc/microshift/config.yaml
twoNode:
  enabled: false                    # Enable 2-node HA mode
  role: ""                          # "primary" or "secondary" (set during init/join)
  vip: ""                           # Virtual IP address for API server
  vipInterface: ""                  # Network interface for VIP (auto-detect if empty)
  peer:                             # Peer node configuration
    address: ""                     # IP address of the other node
    hostname: ""                    # Hostname of the other node
```

### Modified `storage` section (new)
```yaml
storage:
  backend: "etcd"                   # "etcd" (default) or "kine"
  driver: ""                        # CSI storage driver (existing field)

  # PostgreSQL config (only when backend=kine and twoNode.enabled)
  postgresql:
    host: "localhost"               # PostgreSQL host
    port: 5432                      # PostgreSQL port
    database: "microshift"          # Database name
    user: "microshift"              # Database user
    passwordFile: ""                # Path to file containing password
    sslMode: "verify-full"          # TLS mode
    # Note: credentials auto-generated during init-cluster
```

## Go Struct Changes

### `pkg/config/twonode.go` (new file)
```go
package config

// TwoNodeConfig holds configuration for 2-node HA mode
type TwoNodeConfig struct {
    // Enabled activates 2-node HA mode. When true, MicroShift runs with
    // PostgreSQL-backed storage and leader-elected control plane components.
    Enabled bool `json:"enabled"`

    // Role is set during cluster initialization. "primary" for the first node,
    // "secondary" for the joining node. Do not set manually.
    Role string `json:"role,omitempty"`

    // VIP is the Virtual IP address shared between both nodes.
    // Clients use this address to reach the Kubernetes API server.
    VIP string `json:"vip"`

    // VIPInterface is the network interface to bind the VIP to.
    // If empty, auto-detected from the node's primary IP address.
    VIPInterface string `json:"vipInterface,omitempty"`

    // Peer contains the other node's connection information.
    Peer TwoNodePeer `json:"peer"`
}

// TwoNodePeer describes the other node in the 2-node cluster.
type TwoNodePeer struct {
    // Address is the IP address of the peer node.
    Address string `json:"address"`

    // Hostname is the hostname of the peer node.
    Hostname string `json:"hostname,omitempty"`
}
```

### `pkg/config/storage.go` (modified)
```go
// Add to existing Storage struct:
type Storage struct {
    // Existing fields...
    Driver       []string `json:"driver,omitempty"`
    OptionalCSIComponents []string `json:"optionalCsiComponents,omitempty"`

    // New fields for 2-node HA:
    // Backend selects the storage backend. "etcd" (default) uses the embedded
    // etcd server. "kine" uses Kine with PostgreSQL for 2-node HA.
    Backend string `json:"backend,omitempty"`

    // PostgreSQL configuration. Only used when backend is "kine".
    PostgreSQL *PostgreSQLConfig `json:"postgresql,omitempty"`
}

type PostgreSQLConfig struct {
    Host         string `json:"host"`
    Port         int    `json:"port"`
    Database     string `json:"database"`
    User         string `json:"user"`
    PasswordFile string `json:"passwordFile"`
    SSLMode      string `json:"sslMode"`
}
```

### `pkg/config/config.go` (modified)
```go
type Config struct {
    // ... existing fields ...
    TwoNode TwoNodeConfig `json:"twoNode,omitempty"`
}
```

## Validation Rules

```go
func (c *TwoNodeConfig) validate() error {
    if !c.Enabled {
        return nil
    }
    if c.VIP == "" {
        return fmt.Errorf("twoNode.vip is required when twoNode is enabled")
    }
    if net.ParseIP(c.VIP) == nil {
        return fmt.Errorf("twoNode.vip must be a valid IP address")
    }
    if c.Peer.Address == "" {
        return fmt.Errorf("twoNode.peer.address is required")
    }
    if c.Role != "" && c.Role != "primary" && c.Role != "secondary" {
        return fmt.Errorf("twoNode.role must be 'primary' or 'secondary'")
    }
    return nil
}
```

## Example Complete Config for 2-Node

### demo-1 (`/etc/microshift/config.yaml`)
```yaml
dns:
  baseDomain: microshift.example.com
node:
  hostnameOverride: demo-1
  nodeIP: 192.168.5.107
apiServer:
  subjectAltNames:
    - 192.168.5.200            # VIP
twoNode:
  enabled: true
  role: primary
  vip: "192.168.5.200"
  peer:
    address: "192.168.7.167"
    hostname: "demo-2"
storage:
  backend: kine
  postgresql:
    host: localhost
    port: 5432
    database: microshift
    user: microshift
    passwordFile: /var/lib/microshift/secrets/postgresql/password
    sslMode: verify-full
```

### demo-2 (`/etc/microshift/config.yaml`)
```yaml
dns:
  baseDomain: microshift.example.com
node:
  hostnameOverride: demo-2
  nodeIP: 192.168.7.167
apiServer:
  subjectAltNames:
    - 192.168.5.200            # VIP
twoNode:
  enabled: true
  role: secondary
  vip: "192.168.5.200"
  peer:
    address: "192.168.5.107"
    hostname: "demo-1"
storage:
  backend: kine
  postgresql:
    host: localhost
    port: 5432
    database: microshift
    user: microshift
    passwordFile: /var/lib/microshift/secrets/postgresql/password
    sslMode: verify-full
```

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| CREATE | `pkg/config/twonode.go` | TwoNode config struct + validation |
| MODIFY | `pkg/config/storage.go` | Add Backend and PostgreSQL fields |
| MODIFY | `pkg/config/config.go` | Add TwoNode field to Config |
| MODIFY | `packaging/microshift/config.yaml` | Add twoNode defaults |
