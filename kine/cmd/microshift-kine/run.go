package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/k3s-io/kine/pkg/endpoint"
	kinetls "github.com/k3s-io/kine/pkg/tls"
	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

func NewRunKineCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "run",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.ActiveConfig()
			if err != nil {
				klog.Fatalf("Error in reading and validating MicroShift config: %v", err)
			}

			return RunKine(cfg)
		},
	}

	return cmd
}

// RunKine starts Kine as an etcd-compatible gRPC server backed by PostgreSQL.
// It listens on localhost:2379 with TLS using the same certificate paths as
// microshift-etcd, making it a transparent replacement for kube-apiserver.
func RunKine(cfg *config.Config) error {
	if os.Geteuid() > 0 {
		klog.Fatalf("microshift-kine must be run privileged")
	}

	klog.InfoS("Version", "microshift-kine", KineVersionInfo.String())

	// Build the PostgreSQL connection string
	pgEndpoint, err := buildPostgreSQLEndpoint(cfg)
	if err != nil {
		return fmt.Errorf("failed to build PostgreSQL endpoint: %v", err)
	}

	// Use the same TLS certificates as etcd for the gRPC listener so that
	// kube-apiserver can connect without configuration changes.
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	etcdServingCertDir := cryptomaterial.EtcdServingCertDir(certsDir)
	etcdSignerDir := cryptomaterial.EtcdSignerDir(certsDir)

	serverTLS := kinetls.Config{
		CertFile: cryptomaterial.PeerCertPath(etcdServingCertDir),
		KeyFile:  cryptomaterial.PeerKeyPath(etcdServingCertDir),
		CAFile:   cryptomaterial.CACertPath(etcdSignerDir),
	}

	// Start Kine with PostgreSQL backend
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	klog.Infof("Starting Kine with PostgreSQL backend, listening on tcp://0.0.0.0:2379")

	etcdConfig, err := endpoint.Listen(ctx, endpoint.Config{
		Listener:        "tcp://0.0.0.0:2379",
		Endpoint:        pgEndpoint,
		ServerTLSConfig: serverTLS,
		// NotifyInterval must be > 0 to avoid a panic in rand.Int63n when
		// computing jitter for watch progress notifications. 10 minutes
		// matches the etcd default --experimental-watch-progress-notify-interval.
		NotifyInterval: 10 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("failed to start Kine: %v", err)
	}

	klog.Infof("Kine is ready, advertising endpoints: %v", etcdConfig.Endpoints)

	// Wait for termination signal
	sigTerm := make(chan os.Signal, 1)
	signal.Notify(sigTerm, os.Interrupt, syscall.SIGTERM)
	sig := <-sigTerm
	klog.Infof("microshift-kine received signal %v - stopping", sig)

	cancel()
	return nil
}

// buildPostgreSQLEndpoint constructs the Kine-compatible PostgreSQL DSN from
// the MicroShift configuration. In two-node HA mode, it discovers the
// PostgreSQL primary via Patroni so that both nodes' Kine instances connect
// to the writable database regardless of which node is the PG primary.
func buildPostgreSQLEndpoint(cfg *config.Config) (string, error) {
	pgCfg := cfg.Storage.PostgreSQL
	if pgCfg == nil {
		return "", fmt.Errorf("storage.postgresql configuration is required when using kine backend")
	}

	// Read the password from the password file if specified
	password := ""
	if pgCfg.PasswordFile != "" {
		data, err := os.ReadFile(pgCfg.PasswordFile)
		if err != nil {
			// During init-cluster the password file may not exist yet; allow
			// empty password for local peer/socket authentication.
			klog.Warningf("failed to read PostgreSQL password file %s: %v", pgCfg.PasswordFile, err)
		} else {
			password = strings.TrimSpace(string(data))
		}
	}

	// Use 127.0.0.1 instead of "localhost" to avoid IPv6 resolution issues
	// (::1 may not be bound by PostgreSQL if it only listens on IPv4).
	host := pgCfg.Host
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}

	// In two-node HA mode, discover the PostgreSQL primary via Patroni.
	// The PG replica is read-only — Kine must always connect to the primary.
	// On Patroni failover, the connection breaks, MicroShift restarts, and
	// Kine re-discovers the new primary on the next startup.
	if cfg.TwoNode.Enabled {
		primaryHost, err := discoverPatroniPrimary(cfg)
		if err != nil {
			klog.Warningf("Failed to discover PostgreSQL primary via Patroni: %v; falling back to %s", err, host)
		} else {
			klog.Infof("Patroni discovery: PostgreSQL primary is at %s", primaryHost)
			host = primaryHost
		}
	}

	port := pgCfg.Port
	if port == 0 {
		port = 5432
	}
	database := pgCfg.Database
	if database == "" {
		database = "microshift"
	}
	user := pgCfg.User
	if user == "" {
		user = "microshift"
	}
	// Default to sslmode=disable for local connections. PostgreSQL TLS
	// requires additional certificate setup; for same-host Kine→PG
	// connections, the traffic never leaves the machine.
	sslMode := pgCfg.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}

	// Build the PostgreSQL connection string for Kine.
	// Kine expects a standard PostgreSQL DSN prefixed with "postgres://".
	var dsn string
	if password != "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
			user, password, host, port, database, sslMode)
	} else {
		dsn = fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=%s",
			user, host, port, database, sslMode)
	}

	// Append TLS certificate paths for database connection if available
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	pgCertsDir := filepath.Join(certsDir, "postgresql")
	if _, err := os.Stat(pgCertsDir); err == nil {
		certFile := filepath.Join(pgCertsDir, "client.crt")
		keyFile := filepath.Join(pgCertsDir, "client.key")
		caFile := filepath.Join(pgCertsDir, "ca.crt")
		dsn += fmt.Sprintf("&sslcert=%s&sslkey=%s&sslrootcert=%s", certFile, keyFile, caFile)
	}

	return dsn, nil
}

// discoverPatroniPrimary queries the Patroni REST API on the local node and
// the peer node to find which is the PostgreSQL primary. Returns the IP
// address of the primary node's PostgreSQL instance.
func discoverPatroniPrimary(cfg *config.Config) (string, error) {
	// Build the list of candidate nodes: local node IP + peer IP.
	localIP := cfg.Node.NodeIP
	peerIP := cfg.TwoNode.Peer.Address
	candidates := []string{localIP, peerIP}

	client := &http.Client{Timeout: 3 * time.Second}

	for _, ip := range candidates {
		url := fmt.Sprintf("http://%s:8008/patroni", ip)
		resp, err := client.Get(url)
		if err != nil {
			klog.V(2).Infof("Patroni on %s not reachable: %v", ip, err)
			continue
		}
		defer resp.Body.Close()

		var status struct {
			Role  string `json:"role"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			klog.Warningf("Failed to decode Patroni response from %s: %v", ip, err)
			continue
		}

		klog.Infof("Patroni on %s: role=%s state=%s", ip, status.Role, status.State)
		if status.Role == "primary" || status.Role == "master" {
			return ip, nil
		}
	}

	return "", fmt.Errorf("no Patroni primary found among candidates %v", candidates)
}
