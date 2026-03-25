package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/version"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

const (
	initClusterDefaultTimeout = 10 * time.Minute
	passwordLength            = 32
)

// InitClusterOptions holds the flags for the init-cluster command.
type InitClusterOptions struct {
	Timeout time.Duration
}

// JoinToken is the structure encoded into the base64 join token that the
// secondary node uses to bootstrap into the cluster.
type JoinToken struct {
	// PrimaryAddress is the IP of the primary node.
	PrimaryAddress string `json:"primaryAddress"`
	// VIP is the cluster virtual IP.
	VIP string `json:"vip"`
	// DatabasePassword is the Kine database user password.
	DatabasePassword string `json:"databasePassword"`
	// ReplicationPassword is the PostgreSQL replication user password.
	ReplicationPassword string `json:"replicationPassword"`
	// SuperuserPassword is the PostgreSQL superuser password.
	SuperuserPassword string `json:"superuserPassword"`
	// CABundles contains the shared CA signer certificates and keys that
	// must be identical on both nodes. These are used by the secondary
	// node's initCerts() to sign per-node certificates.
	CABundles []CACertBundle `json:"caBundles,omitempty"`
	// ServiceAccountKey contains the shared service account signing key
	// pair. Both API servers must use the same key to validate SA tokens.
	ServiceAccountKey *ServiceAccountKeyBundle `json:"serviceAccountKey,omitempty"`
	// ExpiresAt is the token expiration time.
	ExpiresAt time.Time `json:"expiresAt"`
}

// NewInitClusterCommand creates the "init-cluster" cobra command. This command
// initializes the first node of a 2-node HA MicroShift cluster. It:
//  1. Validates the twoNode configuration in config.yaml.
//  2. Generates PostgreSQL passwords and writes them to disk.
//  3. Produces a join token for the secondary node.
func NewInitClusterCommand() *cobra.Command {
	opts := &InitClusterOptions{
		Timeout: initClusterDefaultTimeout,
	}

	cmd := &cobra.Command{
		Use:   "init-cluster",
		Short: "Initialize the primary node of a 2-node HA cluster",
		Long: `Initializes the first node of a 2-node MicroShift HA cluster.

This command must be run on the primary node before starting MicroShift.
It generates PostgreSQL credentials, writes Patroni configuration, and
produces a join token that the secondary node uses with "join-cluster".

Prerequisites:
  - postgresql-server and patroni must be installed
  - The twoNode section in /etc/microshift/config.yaml must be configured
  - This host must be reachable by the peer node`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitCluster(cmd.Context(), opts)
		},
	}

	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout,
		"Timeout for cluster initialization operations")

	if version.Get().BuildVariant != version.BuildVariantCommunity {
		cmd.Hidden = true
	}

	return cmd
}

func runInitCluster(ctx context.Context, opts *InitClusterOptions) error {
	if os.Geteuid() > 0 {
		return fmt.Errorf("init-cluster must be run as root")
	}

	_, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	klog.Info("Starting 2-node HA cluster initialization...")

	cfg, err := config.ActiveConfig()
	if err != nil {
		return fmt.Errorf("failed to load MicroShift configuration: %w", err)
	}

	if !cfg.TwoNode.Enabled {
		return fmt.Errorf("twoNode.enabled must be true in the configuration to initialize a 2-node cluster")
	}

	if cfg.TwoNode.Role != "" && cfg.TwoNode.Role != config.TwoNodeRolePrimary {
		return fmt.Errorf("this node is configured as %q; init-cluster is only for the primary node", cfg.TwoNode.Role)
	}

	klog.Infof("Configuration validated: VIP=%s, Peer=%s", cfg.TwoNode.VIP, cfg.TwoNode.Peer.Address)

	// Step 1: Generate PostgreSQL passwords
	klog.Info("Generating PostgreSQL credentials...")
	dbPassword, err := generatePassword()
	if err != nil {
		return fmt.Errorf("failed to generate database password: %w", err)
	}
	replPassword, err := generatePassword()
	if err != nil {
		return fmt.Errorf("failed to generate replication password: %w", err)
	}
	suPassword, err := generatePassword()
	if err != nil {
		return fmt.Errorf("failed to generate superuser password: %w", err)
	}

	// Step 2: Write passwords to disk
	secretsDir := filepath.Join(config.DataDir, "secrets", "postgresql")
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	passwordFiles := map[string]string{
		"password":              dbPassword,
		"replication-password":  replPassword,
		"superuser-password":    suPassword,
	}
	for name, value := range passwordFiles {
		path := filepath.Join(secretsDir, name)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
	}
	klog.Info("PostgreSQL credentials written to disk")

	// Step 3: Write the role marker
	roleFile := filepath.Join(config.DataDir, "two-node-role")
	if err := os.WriteFile(roleFile, []byte("primary\n"), 0600); err != nil {
		return fmt.Errorf("failed to write role file: %w", err)
	}

	// Step 4: Collect shared CA certificates and service account keys.
	// These must exist on the primary node before generating the token.
	// If MicroShift has been started at least once, the certs will exist.
	klog.Info("Collecting shared CA certificates...")
	caBundles, err := collectCABundles()
	if err != nil {
		return fmt.Errorf("failed to collect CA bundles: %w", err)
	}
	if len(caBundles) == 0 {
		klog.Warning("No CA certificates found. If MicroShift has not been started yet,")
		klog.Warning("start it once in single-node mode, stop it, then run init-cluster again.")
	} else {
		klog.Infof("Collected %d shared CA signers", len(caBundles))
	}

	var saKey *ServiceAccountKeyBundle
	saKey, err = collectServiceAccountKey()
	if err != nil {
		klog.Warningf("Service account key not found (start MicroShift once first): %v", err)
	} else {
		klog.Info("Collected shared service account signing key")
	}

	// Step 5: Generate join token
	token := JoinToken{
		PrimaryAddress:      cfg.Node.NodeIP,
		VIP:                 cfg.TwoNode.VIP,
		DatabasePassword:    dbPassword,
		ReplicationPassword: replPassword,
		SuperuserPassword:   suPassword,
		CABundles:           caBundles,
		ServiceAccountKey:   saKey,
		ExpiresAt:           time.Now().Add(24 * time.Hour),
	}

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to marshal join token: %w", err)
	}
	tokenB64 := base64.StdEncoding.EncodeToString(tokenJSON)

	// Write token to a file for easy retrieval
	tokenFile := filepath.Join(config.DataDir, "join-token")
	if err := os.WriteFile(tokenFile, []byte(tokenB64), 0600); err != nil {
		return fmt.Errorf("failed to write join token: %w", err)
	}

	klog.Info("Cluster initialization complete.")
	fmt.Println()
	fmt.Println("=== Join Token ===")
	fmt.Println("Run the following on the secondary node:")
	fmt.Println()
	fmt.Printf("  microshift join-cluster --token %s\n", tokenB64)
	fmt.Println()
	fmt.Printf("Token saved to: %s\n", tokenFile)
	fmt.Printf("Token expires: %s\n", token.ExpiresAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("After joining, start MicroShift on both nodes:")
	fmt.Println("  systemctl start microshift")

	return nil
}

// generatePassword creates a cryptographically random password string.
func generatePassword() (string, error) {
	b := make([]byte, passwordLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
