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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	embedded "github.com/openshift/microshift/assets"
	"github.com/openshift/microshift/pkg/config"
	klog "k8s.io/klog/v2"
)

const (
	// keepalivedConfDir is where MicroShift writes the generated keepalived config.
	keepalivedConfDir = "/etc/keepalived"
	// keepalivedConfFile is the configuration file path.
	keepalivedConfFile = "/etc/keepalived/keepalived.conf"
	// healthCheckDir is where the API server health check script is installed.
	healthCheckDir  = "/usr/libexec/microshift"
	healthCheckFile = "/usr/libexec/microshift/check-apiserver.sh"
)

// keepalivedTemplateParams holds the values rendered into the keepalived config template.
type keepalivedTemplateParams struct {
	State           string // "MASTER" or "BACKUP"
	Interface       string // Network interface for VIP (e.g., "eth0")
	VirtualRouterID int    // VRRP virtual router ID (1-255)
	Priority        int    // VRRP priority (higher = preferred master)
	AuthPass        string // VRRP authentication password
	VIP             string // Virtual IP address
	PrefixLen       int    // Subnet prefix length (e.g., 24)
}

// VIPService manages keepalived to provide a floating Virtual IP (VIP) for
// the MicroShift API server in 2-node HA mode. The VIP floats to whichever
// node has a healthy kube-apiserver, providing a stable endpoint for clients.
type VIPService struct {
	cfg *config.Config
}

func NewVIP(cfg *config.Config) *VIPService {
	return &VIPService{cfg: cfg}
}

func (s *VIPService) Name() string           { return "vip" }
func (s *VIPService) Dependencies() []string { return []string{"kube-apiserver"} }

func (s *VIPService) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)

	if !s.cfg.TwoNode.Enabled || s.cfg.TwoNode.VIP == "" {
		klog.Info("VIP service disabled (twoNode not enabled or VIP not set)")
		close(ready)
		<-ctx.Done()
		return ctx.Err()
	}

	// Step 1: Install the health check script
	if err := installHealthCheckScript(); err != nil {
		return fmt.Errorf("failed to install health check script: %w", err)
	}

	// Step 2: Detect the network interface for the VIP
	iface, prefixLen, err := detectVIPInterface(s.cfg)
	if err != nil {
		return fmt.Errorf("failed to detect VIP interface: %w", err)
	}
	klog.Infof("VIP interface detected: %s (prefix /%d)", iface, prefixLen)

	// Step 3: Determine priority based on role
	priority := 100 // primary gets higher priority
	state := "MASTER"
	roleFile := filepath.Join(config.DataDir, "two-node-role")
	if roleData, err := os.ReadFile(roleFile); err == nil {
		if string(bytes.TrimSpace(roleData)) == "secondary" {
			priority = 99
			state = "BACKUP"
		}
	}

	// Step 4: Generate auth password (deterministic from VIP so both nodes match)
	authPass := generateVRRPAuth(s.cfg.TwoNode.VIP)

	// Step 5: Render keepalived configuration
	if err := s.renderKeepalivedConfig(keepalivedTemplateParams{
		State:           state,
		Interface:       iface,
		VirtualRouterID: 51,
		Priority:        priority,
		AuthPass:        authPass,
		VIP:             s.cfg.TwoNode.VIP,
		PrefixLen:       prefixLen,
	}); err != nil {
		return fmt.Errorf("failed to render keepalived config: %w", err)
	}

	// Step 6: Start keepalived
	runningAsSvc := os.Getenv("INVOCATION_ID") != ""
	var exe string
	var args []string

	keepalivedPath, err := exec.LookPath("keepalived")
	if err != nil {
		return fmt.Errorf("keepalived not found in PATH: %w (install with: dnf install keepalived)", err)
	}

	if runningAsSvc {
		if err := stopScopeIfExists("microshift-keepalived.scope"); err != nil {
			return err
		}
		args = append(args,
			"--uid=root",
			"--scope",
			"--collect",
			"--unit", "microshift-keepalived",
			"--property", "Before=microshift.service",
			"--property", "BindsTo=microshift.service",
		)
		args = append(args, keepalivedPath,
			"--dont-fork",
			"--log-console",
			"--use-file", keepalivedConfFile,
		)
		exe = "systemd-run"
	} else {
		exe = keepalivedPath
		args = []string{
			"--dont-fork",
			"--log-console",
			"--use-file", keepalivedConfFile,
		}
	}

	klog.Infof("Starting keepalived: VIP=%s, interface=%s, priority=%d, state=%s",
		s.cfg.TwoNode.VIP, iface, priority, state)
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start keepalived: %w", err)
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			klog.Warningf("keepalived process exited: %v", err)
		}
		klog.Infof("keepalived process quit: %v", cmd.ProcessState.String())

		if !errors.Is(ctx.Err(), context.Canceled) {
			klog.Warning("keepalived terminated prematurely, restarting MicroShift")
			os.Exit(0)
		}
	}()

	defer func() {
		klog.Info("stopping microshift-keepalived")
		stopCmd := exec.Command("systemctl", "stop", "microshift-keepalived.scope", "--no-block")
		if out, err := stopCmd.CombinedOutput(); err != nil {
			klog.ErrorS(err, "failed to stop keepalived", "output", string(out))
		}
	}()

	klog.Info("VIP service is ready")
	close(ready)

	<-ctx.Done()
	return ctx.Err()
}

// renderKeepalivedConfig renders the embedded keepalived template and writes it to disk.
func (s *VIPService) renderKeepalivedConfig(params keepalivedTemplateParams) error {
	tmplBytes, err := embedded.Asset("keepalived/keepalived.conf.tmpl")
	if err != nil {
		return fmt.Errorf("failed to load keepalived template: %w", err)
	}

	tmpl, err := template.New("keepalived.conf").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("failed to parse keepalived template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return fmt.Errorf("failed to render keepalived template: %w", err)
	}

	if err := os.MkdirAll(keepalivedConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create keepalived config directory: %w", err)
	}

	return os.WriteFile(keepalivedConfFile, buf.Bytes(), 0644)
}

// installHealthCheckScript writes the API server health check script to disk.
func installHealthCheckScript() error {
	scriptBytes, err := embedded.Asset("keepalived/check-apiserver.sh")
	if err != nil {
		return fmt.Errorf("failed to load health check script: %w", err)
	}

	if err := os.MkdirAll(healthCheckDir, 0755); err != nil {
		return fmt.Errorf("failed to create health check directory: %w", err)
	}

	return os.WriteFile(healthCheckFile, scriptBytes, 0755)
}

// detectVIPInterface determines the network interface to bind the VIP to.
// If cfg.TwoNode.VIPInterface is set, it uses that. Otherwise, it finds the
// interface that owns the node's primary IP address.
func detectVIPInterface(cfg *config.Config) (string, int, error) {
	if cfg.TwoNode.VIPInterface != "" {
		// User specified the interface — get the prefix length from it
		iface, err := net.InterfaceByName(cfg.TwoNode.VIPInterface)
		if err != nil {
			return "", 0, fmt.Errorf("interface %q not found: %w", cfg.TwoNode.VIPInterface, err)
		}
		prefixLen := 24 // default
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok {
				ones, _ := ipNet.Mask.Size()
				prefixLen = ones
				break
			}
		}
		return cfg.TwoNode.VIPInterface, prefixLen, nil
	}

	// Auto-detect: find the interface that has the node's IP
	nodeIP := net.ParseIP(cfg.Node.NodeIP)
	if nodeIP == nil {
		return "", 0, fmt.Errorf("failed to parse node IP %q", cfg.Node.NodeIP)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", 0, fmt.Errorf("failed to list interfaces: %w", err)
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipNet.IP.Equal(nodeIP) {
				ones, _ := ipNet.Mask.Size()
				return iface.Name, ones, nil
			}
		}
	}

	return "", 0, fmt.Errorf("no interface found with IP %s", cfg.Node.NodeIP)
}

// generateVRRPAuth generates a deterministic 8-character VRRP auth password
// from the VIP address. Both nodes produce the same password because they
// share the same VIP configuration.
func generateVRRPAuth(vip string) string {
	// Use a simple deterministic approach: hash the VIP
	// For production, this could use a shared secret from the join token.
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to VIP-based if crypto/rand fails
		return vip[:8]
	}
	// Actually, for VRRP auth both nodes must use the same password.
	// We derive it from the VIP which is the same on both nodes.
	h := []byte(vip)
	if len(h) > 8 {
		h = h[:8]
	}
	return hex.EncodeToString(h)[:8]
}
