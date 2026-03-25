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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/microshift/pkg/config"
	klog "k8s.io/klog/v2"
)

// failoverState represents the watchdog's current state.
type failoverState int

const (
	failoverHealthy   failoverState = iota // Peer is reachable
	failoverDegraded                       // Peer unreachable, counting failures
	failoverPromoting                      // Executing patronictl failover
	failoverPromoted                       // Promotion complete, monitoring for recovery
)

func (s failoverState) String() string {
	switch s {
	case failoverHealthy:
		return "HEALTHY"
	case failoverDegraded:
		return "DEGRADED"
	case failoverPromoting:
		return "PROMOTING"
	case failoverPromoted:
		return "PROMOTED"
	default:
		return "UNKNOWN"
	}
}

// patroniStatus is the subset of Patroni REST API response we care about.
type patroniStatus struct {
	Role  string `json:"role"`
	State string `json:"state"`
}

// failoverStateFile persists the watchdog's failure count across MicroShift
// restarts. When the PG primary goes down, Kine fails and triggers a
// MicroShift restart. Without persistence, the watchdog would reset to 0
// on each restart and never reach the promotion threshold.
var failoverStateFile = filepath.Join(config.DataDir, "failover-watchdog-state")

// FailoverWatchdog monitors the peer node's Patroni health and triggers
// automatic PostgreSQL failover when the peer is confirmed unreachable
// and the local node is a PG replica. This addresses the 2-node Raft
// quorum limitation where a surviving replica cannot self-promote.
type FailoverWatchdog struct {
	cfg *config.Config

	state         failoverState
	failureCount  int
	httpClient    *http.Client
	peerURL       string
	localURL      string
	checkInterval time.Duration
	threshold     int
}

// RunFailoverPreCheck runs a synchronous failover check before the service
// manager starts. If the peer is unreachable and the accumulated failure count
// (persisted across restarts) exceeds the threshold, it promotes the local PG
// replica directly via pg_ctl promote. This runs BEFORE Kine starts, so Kine
// will connect to the now-local primary on startup.
func RunFailoverPreCheck(cfg *config.Config) {
	w := NewFailoverWatchdog(cfg)

	// If peer is reachable, clear state and return.
	if w.peerIsReachable() {
		if w.failureCount > 0 {
			klog.Infof("Failover pre-check: peer is reachable, clearing %d accumulated failures", w.failureCount)
			w.failureCount = 0
			w.clearState()
		}
		return
	}

	// Peer is unreachable — increment and persist.
	w.failureCount++
	w.saveState()
	klog.Infof("Failover pre-check: peer unreachable (%d/%d)", w.failureCount, w.threshold)

	if w.failureCount < w.threshold {
		return
	}

	// Check local role.
	localRole, err := w.getLocalRole()
	if err != nil {
		klog.Warningf("Failover pre-check: cannot determine local Patroni role: %v", err)
		return
	}

	if localRole == "primary" || localRole == "master" {
		klog.Info("Failover pre-check: local node is already PG primary")
		w.clearState()
		return
	}

	if localRole != "replica" {
		klog.Warningf("Failover pre-check: unexpected local role %q, not promoting", localRole)
		return
	}

	// Promote.
	klog.Warningf("Failover pre-check: peer unreachable for %d restarts, promoting local PG replica", w.failureCount)
	w.promote(context.Background())
}

func NewFailoverWatchdog(cfg *config.Config) *FailoverWatchdog {
	w := &FailoverWatchdog{
		cfg:           cfg,
		state:         failoverHealthy,
		httpClient:    &http.Client{Timeout: 3 * time.Second},
		peerURL:       fmt.Sprintf("http://%s:8008/patroni", cfg.TwoNode.Peer.Address),
		localURL:      fmt.Sprintf("http://%s:8008/patroni", cfg.Node.NodeIP),
		checkInterval: cfg.TwoNode.Failover.EffectiveCheckInterval(),
		threshold:     cfg.TwoNode.Failover.EffectiveFailureThreshold(),
	}
	// Restore failure count from previous run. When the PG primary goes
	// down, Kine crashes and MicroShift restarts — we need to accumulate
	// failures across restarts to reach the promotion threshold.
	w.loadState()
	return w
}

// loadState reads the persisted failure count from disk.
func (w *FailoverWatchdog) loadState() {
	data, err := os.ReadFile(failoverStateFile)
	if err != nil {
		return
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if count > 0 {
		w.failureCount = count
		w.state = failoverDegraded
		klog.Infof("Failover watchdog: restored failure count %d from previous run", count)
	}
}

// saveState persists the current failure count to disk.
func (w *FailoverWatchdog) saveState() {
	_ = os.WriteFile(failoverStateFile, []byte(strconv.Itoa(w.failureCount)), 0600)
}

// clearState removes the persisted failure count (peer is healthy again).
func (w *FailoverWatchdog) clearState() {
	_ = os.Remove(failoverStateFile)
}

func (w *FailoverWatchdog) Name() string { return "failover-watchdog" }

func (w *FailoverWatchdog) Dependencies() []string {
	// No dependencies — the watchdog must start immediately and run
	// independently of the Kine/apiserver lifecycle. When the PG primary
	// goes down, Kine fails and MicroShift restarts. The watchdog needs
	// to accumulate failure counts across restarts, which means it must
	// be able to detect and promote BEFORE Kine tries (and fails) to connect.
	return []string{}
}

func (w *FailoverWatchdog) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)

	klog.Infof("Failover watchdog starting: peer=%s interval=%s threshold=%d failureCount=%d",
		w.cfg.TwoNode.Peer.Address, w.checkInterval, w.threshold, w.failureCount)

	// Signal ready immediately — the watchdog is a background monitor,
	// it doesn't block other services.
	close(ready)

	// Do an immediate check on startup — we may already have accumulated
	// enough failures from previous restarts to trigger promotion.
	w.check(ctx)

	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Info("Failover watchdog stopping")
			return nil
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// check performs one health check cycle against the peer.
func (w *FailoverWatchdog) check(ctx context.Context) {
	// Don't act if we've already promoted.
	if w.state == failoverPromoted {
		// Continue monitoring for peer recovery to log it.
		if w.peerIsReachable() {
			klog.Info("Failover watchdog: peer has recovered after promotion")
		}
		return
	}

	// Don't attempt another promotion if one is in progress.
	if w.state == failoverPromoting {
		return
	}

	peerReachable := w.peerIsReachable()

	if peerReachable {
		if w.state == failoverDegraded {
			klog.Infof("Failover watchdog: peer recovered (was at %d/%d failures), returning to HEALTHY",
				w.failureCount, w.threshold)
		}
		w.state = failoverHealthy
		w.failureCount = 0
		w.clearState()
		return
	}

	// Peer is unreachable.
	w.failureCount++
	w.state = failoverDegraded
	w.saveState()
	klog.Infof("Failover watchdog: peer unreachable (%d/%d)", w.failureCount, w.threshold)

	if w.failureCount < w.threshold {
		return
	}

	// Threshold exceeded — check if we should promote.
	localRole, err := w.getLocalRole()
	if err != nil {
		klog.Warningf("Failover watchdog: cannot determine local Patroni role: %v", err)
		return
	}

	if localRole == "primary" || localRole == "master" {
		klog.Info("Failover watchdog: local node is already PG primary, failsafe_mode handles this case")
		w.state = failoverPromoted
		w.clearState()
		return
	}

	if localRole != "replica" {
		klog.Warningf("Failover watchdog: unexpected local role %q, not promoting", localRole)
		return
	}

	// We are a replica and the peer (primary) is confirmed down. Promote.
	klog.Warningf("Failover watchdog: peer unreachable for %d checks, local role is replica — initiating failover",
		w.failureCount)
	w.state = failoverPromoting
	go w.promote(ctx)
}

// peerIsReachable checks if the peer's Patroni REST API responds.
func (w *FailoverWatchdog) peerIsReachable() bool {
	resp, err := w.httpClient.Get(w.peerURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// getLocalRole queries the local Patroni instance for its current role.
func (w *FailoverWatchdog) getLocalRole() (string, error) {
	resp, err := w.httpClient.Get(w.localURL)
	if err != nil {
		return "", fmt.Errorf("failed to reach local Patroni: %v", err)
	}
	defer resp.Body.Close()

	var status patroniStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("failed to decode Patroni response: %v", err)
	}
	return status.Role, nil
}

// promote directly promotes the local PostgreSQL replica to primary by calling
// pg_ctl promote. This bypasses Patroni's Raft DCS (which has no quorum when
// the peer is down) and promotes PG at the database level. Patroni will detect
// the promotion and update its state accordingly when the DCS recovers.
func (w *FailoverWatchdog) promote(ctx context.Context) {
	klog.Warning("Failover watchdog: promoting local PostgreSQL replica via pg_ctl promote")

	// Find the PostgreSQL data directory from Patroni config.
	pgDataDir := "/var/lib/microshift/postgres/data"

	// pg_ctl promote must run as the postgres user.
	cmd := exec.CommandContext(ctx, "sudo", "-u", "postgres",
		"pg_ctl", "promote", "-D", pgDataDir)

	output, err := cmd.CombinedOutput()
	if err != nil {
		klog.Errorf("Failover watchdog: pg_ctl promote failed: %v\nOutput: %s", err, string(output))
		// Retry on next threshold breach.
		w.state = failoverDegraded
		w.failureCount = 0
		w.saveState()
		return
	}

	klog.Warningf("Failover watchdog: pg_ctl promote succeeded: %s", strings.TrimSpace(string(output)))

	// Wait for PG to finish promotion and verify.
	time.Sleep(5 * time.Second)

	localRole, err := w.getLocalRole()
	if err != nil {
		// Patroni may not reflect the change immediately (DCS is down),
		// but PG itself should now accept writes. Check PG directly.
		klog.Warningf("Failover watchdog: Patroni role check failed (expected — DCS is down): %v", err)
		klog.Info("Failover watchdog: verifying PG is writable directly...")

		if w.pgIsWritable() {
			klog.Info("Failover watchdog: PostgreSQL is writable — promotion confirmed")
			w.state = failoverPromoted
			w.clearState()
		} else {
			klog.Error("Failover watchdog: PostgreSQL is not writable after promotion")
			w.state = failoverDegraded
			w.failureCount = 0
			w.saveState()
		}
		return
	}

	if localRole == "primary" || localRole == "master" {
		klog.Infof("Failover watchdog: promotion verified via Patroni, local role is now %q", localRole)
		w.state = failoverPromoted
		w.clearState()
	} else {
		klog.Warningf("Failover watchdog: Patroni still shows role %q (may update when DCS recovers)", localRole)
		if w.pgIsWritable() {
			klog.Info("Failover watchdog: PostgreSQL is writable — promotion confirmed despite Patroni role")
			w.state = failoverPromoted
			w.clearState()
		} else {
			w.state = failoverDegraded
			w.failureCount = 0
			w.saveState()
		}
	}
}

// pgIsWritable checks if the local PostgreSQL accepts writes by attempting
// a simple query. This works even when Patroni's DCS is down.
func (w *FailoverWatchdog) pgIsWritable() bool {
	cmd := exec.Command("sudo", "-u", "postgres",
		"psql", "-h", "127.0.0.1", "-c", "SELECT pg_is_in_recovery()")
	output, err := cmd.CombinedOutput()
	if err != nil {
		klog.V(2).Infof("Failover watchdog: PG write check failed: %v", err)
		return false
	}
	// pg_is_in_recovery() returns 'f' when the node is primary (not in recovery).
	return strings.Contains(string(output), "f")
}
