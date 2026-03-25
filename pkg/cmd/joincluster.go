package cmd

import (
	"context"
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
	joinClusterDefaultTimeout = 10 * time.Minute
)

// JoinClusterOptions holds the flags for the join-cluster command.
type JoinClusterOptions struct {
	// Token is the base64-encoded join token produced by init-cluster
	// on the primary node. It contains bootstrap credentials and the
	// primary node's address.
	Token string

	// PrimaryAddress overrides the primary node address embedded in the
	// join token. Useful when the primary is reachable via a different
	// address than what was auto-detected.
	PrimaryAddress string

	Timeout time.Duration
}

// NewJoinClusterCommand creates the "join-cluster" cobra command. This command
// joins the secondary node to an existing 2-node HA MicroShift cluster. It:
//  1. Decodes the join token from the primary node.
//  2. Writes shared PostgreSQL passwords to disk.
//  3. Writes the secondary role marker.
//  4. Prepares the node to start MicroShift with Kine + PostgreSQL replica.
func NewJoinClusterCommand() *cobra.Command {
	opts := &JoinClusterOptions{
		Timeout: joinClusterDefaultTimeout,
	}

	cmd := &cobra.Command{
		Use:   "join-cluster",
		Short: "Join this node to a 2-node HA cluster as the secondary",
		Long: `Joins this node to an existing 2-node MicroShift HA cluster.

This command must be run on the secondary node after "init-cluster" has
been run on the primary. It decodes the join token, writes PostgreSQL
credentials, and prepares MicroShift for HA operation.

Prerequisites:
  - postgresql-server and patroni must be installed
  - The twoNode section in /etc/microshift/config.yaml must be configured
  - A valid join token from the primary node's "init-cluster" output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJoinCluster(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.Token, "token", "",
		"Join token from the primary node's init-cluster command")
	cmd.Flags().StringVar(&opts.PrimaryAddress, "primary", "",
		"Override the primary node address (default: from join token)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout,
		"Timeout for cluster join operations")

	if version.Get().BuildVariant != version.BuildVariantCommunity {
		cmd.Hidden = true
	}

	return cmd
}

func runJoinCluster(ctx context.Context, opts *JoinClusterOptions) error {
	if os.Geteuid() > 0 {
		return fmt.Errorf("join-cluster must be run as root")
	}

	_, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	klog.Info("Starting 2-node HA cluster join...")

	cfg, err := config.ActiveConfig()
	if err != nil {
		return fmt.Errorf("failed to load MicroShift configuration: %w", err)
	}

	if !cfg.TwoNode.Enabled {
		return fmt.Errorf("twoNode.enabled must be true in the configuration to join a 2-node cluster")
	}

	if cfg.TwoNode.Role != "" && cfg.TwoNode.Role != config.TwoNodeRoleSecondary {
		return fmt.Errorf("this node is configured as %q; join-cluster is only for the secondary node", cfg.TwoNode.Role)
	}

	if opts.Token == "" {
		return fmt.Errorf("--token is required: provide the join token from the primary node")
	}

	// Step 1: Decode and validate join token
	klog.Info("Decoding join token...")
	tokenJSON, err := base64.StdEncoding.DecodeString(opts.Token)
	if err != nil {
		return fmt.Errorf("invalid join token (base64 decode failed): %w", err)
	}

	var token JoinToken
	if err := json.Unmarshal(tokenJSON, &token); err != nil {
		return fmt.Errorf("invalid join token (JSON parse failed): %w", err)
	}

	if time.Now().After(token.ExpiresAt) {
		return fmt.Errorf("join token expired at %s; run init-cluster again on the primary node", token.ExpiresAt.Format(time.RFC3339))
	}

	primaryAddr := token.PrimaryAddress
	if opts.PrimaryAddress != "" {
		primaryAddr = opts.PrimaryAddress
	}

	klog.Infof("Join token validated: Primary=%s, VIP=%s, Expires=%s",
		primaryAddr, token.VIP, token.ExpiresAt.Format(time.RFC3339))

	// Step 2: Write PostgreSQL passwords from the token
	klog.Info("Writing PostgreSQL credentials...")
	secretsDir := filepath.Join(config.DataDir, "secrets", "postgresql")
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	passwordFiles := map[string]string{
		"password":             token.DatabasePassword,
		"replication-password": token.ReplicationPassword,
		"superuser-password":   token.SuperuserPassword,
	}
	for name, value := range passwordFiles {
		path := filepath.Join(secretsDir, name)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
	}
	klog.Info("PostgreSQL credentials written to disk")

	// Step 3: Write shared CA certificates from the token.
	// These are the signer CA cert+key pairs that must be identical on both
	// nodes so that per-node certificates are trusted cluster-wide.
	if len(token.CABundles) > 0 {
		klog.Infof("Writing %d shared CA certificate bundles...", len(token.CABundles))
		if err := writeCABundles(token.CABundles); err != nil {
			return fmt.Errorf("failed to write CA bundles: %w", err)
		}
		klog.Info("Shared CA certificates written to disk")
	} else {
		klog.Warning("No CA certificates in join token. MicroShift will generate new CAs on first start.")
		klog.Warning("This means the secondary node will NOT trust the primary's certificates.")
		klog.Warning("Re-run init-cluster on the primary after starting MicroShift once.")
	}

	// Step 4: Write shared service account signing key.
	if token.ServiceAccountKey != nil {
		klog.Info("Writing shared service account signing key...")
		if err := writeServiceAccountKey(token.ServiceAccountKey); err != nil {
			return fmt.Errorf("failed to write service account key: %w", err)
		}
	}

	// Step 5: Write the role marker
	roleFile := filepath.Join(config.DataDir, "two-node-role")
	if err := os.WriteFile(roleFile, []byte("secondary\n"), 0600); err != nil {
		return fmt.Errorf("failed to write role file: %w", err)
	}

	klog.Info("Join complete. Start MicroShift with 'systemctl start microshift'.")
	fmt.Println()
	fmt.Println("Secondary node configured successfully.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Start MicroShift on the primary node:  systemctl start microshift")
	fmt.Println("  2. Start MicroShift on this node:          systemctl start microshift")
	fmt.Println()
	fmt.Println("Patroni will automatically configure PostgreSQL streaming replication")
	fmt.Println("when both nodes start.")

	return nil
}
