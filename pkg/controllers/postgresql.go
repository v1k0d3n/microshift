/*
Copyright © 2024 MicroShift Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package controllers

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	embedded "github.com/openshift/microshift/assets"
	"github.com/openshift/microshift/pkg/config"
	klog "k8s.io/klog/v2"
)

const (
	// Patroni with Raft DCS needs both nodes online before it can form quorum
	// and bootstrap PostgreSQL. Allow generous time for the peer to start.
	patroniHealthCheckRetries = 60
	patroniHealthCheckWait    = 5 * time.Second
)

// patroniTemplateParams holds the values rendered into the Patroni YAML template.
type patroniTemplateParams struct {
	Hostname            string
	NodeIP              string
	PeerIP              string
	DataDir             string
	Port                int
	Database            string
	DatabaseUser        string
	DatabasePassword    string
	ReplicationUser     string
	ReplicationPassword string
	SuperuserPassword   string
	// IncludePartner controls whether the Raft partner_addrs is included.
	// On the primary node's first bootstrap (no existing Raft state), this
	// is false so Patroni self-elects as leader. On subsequent starts and
	// on the secondary node, this is true.
	IncludePartner bool
}

// PostgreSQLService manages the PostgreSQL instance via Patroni for 2-node HA.
// Patroni handles PostgreSQL lifecycle, streaming replication, and automatic
// failover. This service renders the Patroni configuration from a template,
// starts the Patroni process, and waits for PostgreSQL to become ready.
type PostgreSQLService struct {
	cfg *config.Config
}

func NewPostgreSQL(cfg *config.Config) *PostgreSQLService {
	return &PostgreSQLService{cfg: cfg}
}

func (s *PostgreSQLService) Name() string           { return "postgresql" }
func (s *PostgreSQLService) Dependencies() []string { return []string{} }

func (s *PostgreSQLService) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)

	pgCfg := s.cfg.Storage.PostgreSQL
	if pgCfg == nil {
		return fmt.Errorf("storage.postgresql configuration is required for PostgreSQL service")
	}

	// Ensure directories exist with correct ownership.
	// PostgreSQL refuses to run with data directories owned by root — they
	// must be owned by the "postgres" system user.
	patroniCfgDir := filepath.Join(config.DataDir, "patroni")
	if err := os.MkdirAll(patroniCfgDir, 0750); err != nil {
		return fmt.Errorf("failed to create patroni config directory: %w", err)
	}
	pgDataParent := filepath.Join(config.DataDir, "postgres")
	if err := os.MkdirAll(pgDataParent, 0750); err != nil {
		return fmt.Errorf("failed to create postgres directory: %w", err)
	}
	// The postgres user needs to traverse into /var/lib/microshift to reach
	// its data and config directories. Set the parent to world-executable
	// (o+x) so the postgres user can cd into it, without exposing file contents.
	if err := os.Chmod(config.DataDir, 0711); err != nil {
		klog.Warningf("failed to chmod datadir: %v", err)
	}

	// Set postgres ownership on all PG-related directories
	if err := chownPostgres(pgDataParent); err != nil {
		return fmt.Errorf("failed to set postgres ownership: %w", err)
	}
	if err := chownPostgres(patroniCfgDir); err != nil {
		return fmt.Errorf("failed to set patroni ownership: %w", err)
	}
	raftDir := filepath.Join(patroniCfgDir, "raft")
	if err := os.MkdirAll(raftDir, 0750); err != nil {
		return fmt.Errorf("failed to create raft directory: %w", err)
	}
	if err := chownPostgres(raftDir); err != nil {
		return fmt.Errorf("failed to set raft ownership: %w", err)
	}

	// Render Patroni configuration
	patroniCfgPath := filepath.Join(patroniCfgDir, "patroni.yml")
	if err := s.renderPatroniConfig(patroniCfgPath); err != nil {
		return fmt.Errorf("failed to render patroni configuration: %w", err)
	}
	// Config must be readable by postgres user
	if err := chownPostgres(patroniCfgPath); err != nil {
		klog.Warningf("failed to chown patroni config: %v", err)
	}
	klog.Infof("Patroni configuration written to %s", patroniCfgPath)

	// Ensure password files are readable by postgres user
	secretsDir := filepath.Join(config.DataDir, "secrets", "postgresql")
	if err := chownPostgres(secretsDir); err != nil {
		klog.Warningf("failed to chown postgresql secrets: %v", err)
	}

	// Start Patroni
	runningAsSvc := os.Getenv("INVOCATION_ID") != ""
	var exe string
	var args []string

	patroniPath, err := exec.LookPath("patroni")
	if err != nil {
		return fmt.Errorf("patroni not found in PATH: %w (install with: dnf install patroni)", err)
	}

	// Patroni must run as the "postgres" user because PostgreSQL refuses to
	// start as root. When running as a systemd service, we use --uid=postgres.
	// When running standalone, we use sudo -u postgres.
	if runningAsSvc {
		if err := stopScopeIfExists("microshift-patroni.scope"); err != nil {
			return err
		}
		args = append(args,
			"--uid=postgres",
			"--gid=postgres",
			"--scope",
			"--collect",
			"--unit", "microshift-patroni",
			"--property", "Before=microshift.service",
			"--property", "BindsTo=microshift.service",
		)
		args = append(args, patroniPath, patroniCfgPath)
		exe = "systemd-run"
	} else {
		exe = "sudo"
		args = []string{"-u", "postgres", patroniPath, patroniCfgPath}
	}

	klog.Infof("Starting Patroni via %s with config %s", exe, patroniCfgPath)
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Patroni: %w", err)
	}

	// Monitor Patroni process — unlike etcd/kine, we do NOT os.Exit when
	// Patroni exits. Patroni may briefly exit during Raft leader election
	// or PG restart cycles, especially on first bootstrap. The health check
	// (waitForPostgresReady) handles the retry. If Patroni truly can't
	// start, the health check will eventually time out and the service
	// will fail gracefully.
	patroniExited := make(chan struct{})
	go func() {
		if err := cmd.Wait(); err != nil {
			klog.Warningf("Patroni process exited: %v", err)
		}
		klog.Infof("Patroni process quit: %v", cmd.ProcessState.String())
		close(patroniExited)
	}()

	defer func() {
		klog.Info("stopping microshift-patroni")
		stopCmd := exec.Command("systemctl", "stop", "microshift-patroni.scope", "--no-block")
		if out, err := stopCmd.CombinedOutput(); err != nil {
			klog.ErrorS(err, "failed to stop microshift-patroni", "output", string(out))
		}
	}()

	// Wait for PostgreSQL to accept connections
	if err := s.waitForPostgresReady(ctx); err != nil {
		return err
	}

	// Ensure the database exists
	if err := s.ensureDatabase(ctx); err != nil {
		return err
	}

	klog.Info("PostgreSQL is ready!")
	close(ready)

	<-ctx.Done()
	return ctx.Err()
}

// renderPatroniConfig renders the embedded Patroni YAML template with the
// current node's configuration values and writes it to disk.
func (s *PostgreSQLService) renderPatroniConfig(destPath string) error {
	tmplBytes, err := embedded.Asset("postgresql/patroni.yml.tmpl")
	if err != nil {
		return fmt.Errorf("failed to load patroni template: %w", err)
	}

	pgCfg := s.cfg.Storage.PostgreSQL

	// Read passwords
	dbPassword := readPasswordFile(pgCfg.PasswordFile)
	replPassword := readPasswordFile(filepath.Join(config.DataDir, "secrets", "postgresql", "replication-password"))
	suPassword := readPasswordFile(filepath.Join(config.DataDir, "secrets", "postgresql", "superuser-password"))

	// Determine whether to include the Raft partner in the config.
	// On the primary node's first bootstrap (no existing Raft state dir),
	// we start Patroni solo so it self-elects as leader and bootstraps PG.
	// On the secondary and on all subsequent starts, include the partner
	// so Raft consensus works normally.
	raftStateDir := filepath.Join(config.DataDir, "patroni", "raft")
	includePartner := true
	if s.cfg.TwoNode.Role == config.TwoNodeRolePrimary {
		// Check if Raft state already exists (non-empty dir)
		entries, _ := os.ReadDir(raftStateDir)
		if len(entries) == 0 {
			klog.Info("Primary node first bootstrap: starting Patroni without Raft partner for self-election")
			includePartner = false
		}
	}

	params := patroniTemplateParams{
		Hostname:            s.cfg.Node.HostnameOverride,
		NodeIP:              s.cfg.Node.NodeIP,
		PeerIP:              s.cfg.TwoNode.Peer.Address,
		DataDir:             config.DataDir,
		Port:                pgCfg.Port,
		Database:            pgCfg.Database,
		DatabaseUser:        pgCfg.User,
		DatabasePassword:    dbPassword,
		ReplicationUser:     "replicator",
		ReplicationPassword: replPassword,
		SuperuserPassword:   suPassword,
		IncludePartner:      includePartner,
	}

	tmpl, err := template.New("patroni.yml").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("failed to parse patroni template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("failed to render patroni template: %w", err)
	}

	return os.WriteFile(destPath, buf.Bytes(), 0600)
}

// waitForPostgresReady polls the local PostgreSQL instance until it accepts
// TCP connections on the configured port.
func (s *PostgreSQLService) waitForPostgresReady(ctx context.Context) error {
	pgCfg := s.cfg.Storage.PostgreSQL
	addr := fmt.Sprintf("127.0.0.1:%d", pgCfg.Port)

	for attempt := 0; attempt < patroniHealthCheckRetries; attempt++ {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			_ = conn.Close()
			klog.Infof("PostgreSQL is accepting connections on %s", addr)
			return nil
		}
		klog.Infof("Waiting for PostgreSQL on %s (attempt %d/%d): %v", addr, attempt+1, patroniHealthCheckRetries, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(patroniHealthCheckWait):
		}
	}
	return fmt.Errorf("PostgreSQL did not become ready on %s after %d attempts", addr, patroniHealthCheckRetries)
}

// ensureDatabase creates the Kine database role and database if they don't
// already exist. This uses the psql command-line tool to avoid adding a Go
// PostgreSQL driver dependency to the main MicroShift module.
//
// On a replica node, these commands will fail (read-only) which is fine —
// the role and database replicate automatically from the primary via
// streaming replication.
func (s *PostgreSQLService) ensureDatabase(ctx context.Context) error {
	pgCfg := s.cfg.Storage.PostgreSQL
	port := fmt.Sprintf("%d", pgCfg.Port)
	suPassword := readPasswordFile(filepath.Join(config.DataDir, "secrets", "postgresql", "superuser-password"))
	dbPassword := readPasswordFile(pgCfg.PasswordFile)

	pgEnv := append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", suPassword))

	// Helper to run psql as the postgres superuser
	runPsql := func(sql string) (string, error) {
		cmd := exec.CommandContext(ctx, "psql",
			"-h", "127.0.0.1",
			"-p", port,
			"-U", "postgres",
			"-tAc", sql,
		)
		cmd.Env = pgEnv
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Retry a few times — PostgreSQL may still be starting up even though
	// the TCP port is open (Patroni health checks can pass before PG is
	// fully ready to accept authenticated connections).
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		// Step 1: Create role if not exists
		roleExists, err := runPsql(fmt.Sprintf("SELECT 1 FROM pg_roles WHERE rolname='%s'", pgCfg.User))
		if err != nil {
			lastErr = fmt.Errorf("checking role: %w", err)
			klog.Infof("Waiting for PostgreSQL to accept queries (attempt %d/10): %v", attempt+1, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}

		if roleExists != "1" {
			klog.Infof("Creating PostgreSQL role %q", pgCfg.User)
			// Use dollar-quoting to avoid password escaping issues
			createRoleSQL := fmt.Sprintf(
				"CREATE ROLE %s WITH LOGIN PASSWORD '%s' CREATEDB CREATEROLE",
				pgCfg.User, dbPassword,
			)
			if out, err := runPsql(createRoleSQL); err != nil {
				// On a replica, this fails with "read-only transaction" — that's expected
				if strings.Contains(out, "read-only") || strings.Contains(out, "recovery") {
					klog.Info("This node is a PostgreSQL replica — role/database will replicate from primary")
					return nil
				}
				klog.Warningf("Failed to create role %q: %v (output: %s)", pgCfg.User, err, out)
			} else {
				klog.Infof("PostgreSQL role %q created", pgCfg.User)
			}
		} else {
			klog.Infof("PostgreSQL role %q already exists", pgCfg.User)
		}

		// Step 2: Create database if not exists
		dbExists, err := runPsql(fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname='%s'", pgCfg.Database))
		if err != nil {
			lastErr = fmt.Errorf("checking database: %w", err)
			continue
		}

		if dbExists != "1" {
			klog.Infof("Creating PostgreSQL database %q", pgCfg.Database)
			if out, err := runPsql(fmt.Sprintf("CREATE DATABASE %s OWNER %s", pgCfg.Database, pgCfg.User)); err != nil {
				if strings.Contains(out, "read-only") || strings.Contains(out, "recovery") {
					klog.Info("This node is a PostgreSQL replica — database will replicate from primary")
					return nil
				}
				klog.Warningf("Failed to create database %q: %v (output: %s)", pgCfg.Database, err, out)
			} else {
				klog.Infof("PostgreSQL database %q created", pgCfg.Database)
			}
		} else {
			klog.Infof("PostgreSQL database %q already exists", pgCfg.Database)
		}

		return nil
	}

	// If we got here, all retries failed — but don't block startup.
	// The role/db may have been created by the primary and replicated.
	klog.Warningf("Could not verify database setup after retries: %v", lastErr)
	return nil
}

// stopScopeIfExists checks if a systemd scope is active and stops it.
func stopScopeIfExists(scope string) error {
	statusCmd := exec.Command("systemctl", "status", scope)
	if err := statusCmd.Run(); err != nil {
		//nolint:nilerr
		return nil
	}

	klog.InfoS("scope is already active - stopping", "scope", scope)
	stopCmd := exec.Command("systemctl", "stop", scope)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		klog.ErrorS(err, "failed to stop scope", "scope", scope, "output", string(out))
		return err
	}
	return nil
}

// readPasswordFile reads a password from a file, trimming whitespace.
// Returns an empty string if the file cannot be read.
func readPasswordFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// chownPostgres recursively sets ownership of a path to the "postgres" system user.
func chownPostgres(path string) error {
	u, err := user.Lookup("postgres")
	if err != nil {
		return fmt.Errorf("postgres user not found: %w (install postgresql-server)", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return syscall.Chown(name, uid, gid)
	})
}
