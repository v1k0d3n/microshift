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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	klog "k8s.io/klog/v2"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// KineService manages the microshift-kine subprocess, which provides an
// etcd-compatible gRPC API backed by PostgreSQL via Kine. It follows the
// same process-management pattern as EtcdService.
type KineService struct {
	cfg *config.Config
}

func NewKine(cfg *config.Config) *KineService {
	return &KineService{
		cfg: cfg,
	}
}

func (s *KineService) Name() string { return "kine" }

// Dependencies returns an empty list. PostgreSQL is managed externally (via
// Patroni/systemd) and must be running before MicroShift starts, following
// the K3s model where the database is the user's responsibility.
func (s *KineService) Dependencies() []string { return []string{} }

func (s *KineService) Run(ctx context.Context, ready chan<- struct{}, stopped chan<- struct{}) error {
	defer close(stopped)

	// Check to see if we should run as a systemd scope or directly as a binary.
	runningAsSvc := os.Getenv("INVOCATION_ID") != ""

	// Get the path to the kine binary based on the MicroShift binary location.
	microshiftExecPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%v failed to get exec path: %v", s.Name(), err)
	}
	kinePath := filepath.Join(filepath.Dir(microshiftExecPath), "microshift-kine")
	args := []string{}

	// If running as a systemd service, wrap kine in a transient systemd scope
	// tied to the MicroShift service lifetime (same pattern as etcd).
	var exe string
	if runningAsSvc {
		if err := stopMicroshiftKineScopeIfExists(); err != nil {
			return err
		}

		args = append(args,
			"--uid=root",
			"--scope",
			"--collect",
			"--unit", "microshift-kine",
			"--property", "Before=microshift.service",
			"--property", "BindsTo=microshift.service",
		)

		args = append(args, kinePath)
		exe = "systemd-run"
	} else {
		exe = kinePath
	}
	args = append(args, "run")

	klog.Infof("starting kine via %s with args %v", exe, args)
	cmd := exec.Command(exe, args...)

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("%s failed to get workdir: %v", s.Name(), err)
	}
	cmd.Dir = wd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("%s failed to start: %v", s.Name(), err)
	}

	// Handle microshift-kine termination before microshift process exits
	go func() {
		if err := cmd.Wait(); err != nil {
			klog.Warningf("%v failed waiting on process to finish: %+v", s.Name(), err)
		}
		klog.Infof("%v process quit: %v", s.Name(), cmd.ProcessState.String())

		if !errors.Is(ctx.Err(), context.Canceled) {
			// Exit microshift to trigger microshift-kine restart
			klog.Warning("microshift-kine process terminated prematurely, restarting MicroShift")
			os.Exit(0)
		} else {
			klog.Info("MicroShift is mid shutdown - ignoring kine termination")
		}
	}()

	// Ensures microshift-kine scope is stopped after microshift
	defer func() {
		klog.Info("stopping microshift-kine")
		cmd := exec.Command("systemctl", "stop", "microshift-kine.scope", "--no-block")

		if out, err := cmd.CombinedOutput(); err != nil {
			klog.ErrorS(err, "failed to stop microshift-kine", "output", string(out))
			return
		}
	}()

	// Kine speaks the etcd v3 gRPC protocol on the same port (2379), so we
	// reuse the same health check logic as etcd.
	if err := checkIfKineIsReady(ctx); err != nil {
		return err
	}
	klog.Info("kine is ready!")
	close(ready)

	// Wait for MicroShift to be done
	<-ctx.Done()
	return ctx.Err()
}

func stopMicroshiftKineScopeIfExists() error {
	statusCmd := exec.Command("systemctl", "status", "microshift-kine.scope")
	if err := statusCmd.Run(); err != nil {
		//nolint:nilerr
		return nil
	}

	klog.InfoS("microshift-kine.scope is already active - stopping")
	stopCmd := exec.Command("systemctl", "stop", "microshift-kine.scope")
	if out, err := stopCmd.CombinedOutput(); err != nil {
		klog.ErrorS(err, "failed to stop microshift-kine", "output", string(out))
		return err
	}
	return nil
}

// checkIfKineIsReady verifies that Kine's etcd-compatible gRPC endpoint is
// accepting connections and responding to requests. The check uses the same
// etcd client library and certificate paths as the etcd health check.
func checkIfKineIsReady(ctx context.Context) error {
	var client *clientv3.Client
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()

	for attempt := 0; attempt < HealthCheckRetries; attempt++ {
		if client == nil {
			var err error
			client, err = getKineClient(ctx)
			if err != nil {
				klog.Infof("failed to obtain kine client: %v", err)
				if attempt < HealthCheckRetries-1 {
					time.Sleep(HealthCheckWait)
					continue
				}
				return fmt.Errorf("failed to obtain kine client after %d attempts: %v", HealthCheckRetries, err)
			}
		}

		// Kine implements the etcd Status RPC. A successful call means the
		// gRPC server is up and the PostgreSQL backend is connected.
		_, err := client.Status(ctx, "127.0.0.1:2379")
		if err != nil {
			_ = client.Close()
			client = nil
			klog.Infof("failed to get kine status: %v", err)
			if err == context.Canceled {
				return err
			}
			if attempt < HealthCheckRetries-1 {
				time.Sleep(HealthCheckWait)
				continue
			}
			return fmt.Errorf("failed to get kine status after %d attempts: %v", HealthCheckRetries, err)
		}

		// Additionally try a Get to ensure the PostgreSQL backend is functional.
		if _, err = client.Get(ctx, "health"); err == nil {
			return nil
		}
		_ = client.Close()
		client = nil
		klog.Infof("kine not ready yet: %v", err)
		if err == context.Canceled {
			return err
		}

		if attempt < HealthCheckRetries-1 {
			time.Sleep(HealthCheckWait)
		}
	}
	return fmt.Errorf("kine still not healthy after checking %d times", HealthCheckRetries)
}

// getKineClient creates an etcd v3 client configured to talk to the local Kine
// instance. It reuses getEtcdClient since Kine speaks the same protocol on the
// same port with the same TLS certificates. This function exists as a named
// alias for documentation clarity.
func getKineClient(ctx context.Context) (*clientv3.Client, error) {
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	etcdAPIServerClientCertDir := cryptomaterial.EtcdAPIServerClientCertDir(certsDir)

	tlsInfo := transport.TLSInfo{
		CertFile:      cryptomaterial.ClientCertPath(etcdAPIServerClientCertDir),
		KeyFile:       cryptomaterial.ClientKeyPath(etcdAPIServerClientCertDir),
		TrustedCAFile: cryptomaterial.CACertPath(cryptomaterial.EtcdSignerDir(certsDir)),
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"https://127.0.0.1:2379"},
		DialTimeout: 100 * time.Millisecond,
		TLS:         tlsConfig,
		Context:     ctx,
	})
	if err != nil {
		return nil, err
	}
	return cli, nil
}
