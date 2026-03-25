package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift/microshift/pkg/config"
	"github.com/openshift/microshift/pkg/util/cryptomaterial"
	"k8s.io/klog/v2"
)

// CACertBundle holds a single CA signer's certificate and key material
// for distribution between nodes.
type CACertBundle struct {
	// Name is a human-readable identifier (e.g., "kube-control-plane-signer").
	Name string `json:"name"`
	// RelDir is the relative path under the certs directory where these
	// files should be written (e.g., "kube-control-plane-signer").
	RelDir string `json:"relDir"`
	// CACert is the base64-encoded CA certificate (ca.crt).
	CACert string `json:"caCert"`
	// CAKey is the base64-encoded CA private key (ca.key).
	CAKey string `json:"caKey"`
}

// ServiceAccountKeyBundle holds the service account signing key pair.
type ServiceAccountKeyBundle struct {
	// PublicKey is the base64-encoded service-account.pub.
	PublicKey string `json:"publicKey"`
	// PrivateKey is the base64-encoded service-account.key.
	PrivateKey string `json:"privateKey"`
}

// sharedCASignerDirs returns the list of signer directory functions and their
// relative paths under the certs directory. These are the CAs that must be
// identical on both nodes in a 2-node cluster.
func sharedCASignerDirs(certsDir string) []struct {
	name   string
	relDir string
	dir    string
} {
	return []struct {
		name   string
		relDir string
		dir    string
	}{
		{"kube-control-plane-signer", "kube-control-plane-signer", cryptomaterial.KubeControlPlaneSignerCertDir(certsDir)},
		{"kube-apiserver-to-kubelet-signer", "kube-apiserver-to-kubelet-client-signer", cryptomaterial.KubeAPIServerToKubeletSignerCertDir(certsDir)},
		{"admin-kubeconfig-signer", "admin-kubeconfig-signer", cryptomaterial.AdminKubeconfigSignerDir(certsDir)},
		{"kubelet-csr-signer-signer", "kubelet-csr-signer-signer", cryptomaterial.KubeletCSRSignerSignerCertDir(certsDir)},
		{"csr-signer", "kubelet-csr-signer-signer/csr-signer", cryptomaterial.CSRSignerCertDir(certsDir)},
		{"aggregator-signer", "aggregator-signer", cryptomaterial.AggregatorSignerDir(certsDir)},
		{"service-ca", "service-ca", cryptomaterial.ServiceCADir(certsDir)},
		{"ingress-ca", "ingress-ca", cryptomaterial.IngressCADir(certsDir)},
		{"kube-apiserver-external-signer", "kube-apiserver-external-signer", cryptomaterial.KubeAPIServerExternalSigner(certsDir)},
		{"kube-apiserver-localhost-signer", "kube-apiserver-localhost-signer", cryptomaterial.KubeAPIServerLocalhostSigner(certsDir)},
		{"kube-apiserver-service-network-signer", "kube-apiserver-service-network-signer", cryptomaterial.KubeAPIServerServiceNetworkSigner(certsDir)},
		{"etcd-signer", "etcd-signer", cryptomaterial.EtcdSignerDir(certsDir)},
	}
}

// collectCABundles reads all shared CA signer cert+key pairs from disk and
// returns them as base64-encoded bundles ready for inclusion in the join token.
func collectCABundles() ([]CACertBundle, error) {
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)
	signers := sharedCASignerDirs(certsDir)

	bundles := make([]CACertBundle, 0, len(signers))
	for _, s := range signers {
		certPath := cryptomaterial.CACertPath(s.dir)
		keyPath := cryptomaterial.CAKeyPath(s.dir)

		certData, err := os.ReadFile(certPath)
		if err != nil {
			// Some signers may not exist yet if init-cluster runs before MicroShift
			// has generated certs. Skip missing ones.
			klog.V(2).Infof("Skipping CA %s: %v", s.name, err)
			continue
		}

		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			klog.V(2).Infof("Skipping CA key %s: %v", s.name, err)
			continue
		}

		bundles = append(bundles, CACertBundle{
			Name:   s.name,
			RelDir: s.relDir,
			CACert: base64.StdEncoding.EncodeToString(certData),
			CAKey:  base64.StdEncoding.EncodeToString(keyData),
		})
	}

	return bundles, nil
}

// collectServiceAccountKey reads the service account signing key pair from disk.
func collectServiceAccountKey() (*ServiceAccountKeyBundle, error) {
	saKeyDir := filepath.Join(config.DataDir, "resources", "kube-apiserver", "secrets", "service-account-key")
	pubData, err := os.ReadFile(filepath.Join(saKeyDir, "service-account.pub"))
	if err != nil {
		return nil, fmt.Errorf("failed to read service-account.pub: %w", err)
	}
	keyData, err := os.ReadFile(filepath.Join(saKeyDir, "service-account.key"))
	if err != nil {
		return nil, fmt.Errorf("failed to read service-account.key: %w", err)
	}

	return &ServiceAccountKeyBundle{
		PublicKey:  base64.StdEncoding.EncodeToString(pubData),
		PrivateKey: base64.StdEncoding.EncodeToString(keyData),
	}, nil
}

// writeCABundles writes the CA signer cert+key pairs from the join token
// to the appropriate locations on disk.
func writeCABundles(bundles []CACertBundle) error {
	certsDir := cryptomaterial.CertsDirectory(config.DataDir)

	for _, b := range bundles {
		dir := filepath.Join(certsDir, b.RelDir)
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		certData, err := base64.StdEncoding.DecodeString(b.CACert)
		if err != nil {
			return fmt.Errorf("failed to decode CA cert for %s: %w", b.Name, err)
		}
		keyData, err := base64.StdEncoding.DecodeString(b.CAKey)
		if err != nil {
			return fmt.Errorf("failed to decode CA key for %s: %w", b.Name, err)
		}

		if err := os.WriteFile(cryptomaterial.CACertPath(dir), certData, 0600); err != nil {
			return fmt.Errorf("failed to write CA cert for %s: %w", b.Name, err)
		}
		if err := os.WriteFile(cryptomaterial.CAKeyPath(dir), keyData, 0600); err != nil {
			return fmt.Errorf("failed to write CA key for %s: %w", b.Name, err)
		}

		klog.V(2).Infof("Wrote shared CA: %s → %s", b.Name, dir)
	}

	return nil
}

// writeServiceAccountKey writes the service account key pair from the join token.
func writeServiceAccountKey(saKey *ServiceAccountKeyBundle) error {
	if saKey == nil {
		return nil
	}

	saKeyDir := filepath.Join(config.DataDir, "resources", "kube-apiserver", "secrets", "service-account-key")
	if err := os.MkdirAll(saKeyDir, 0750); err != nil {
		return fmt.Errorf("failed to create SA key directory: %w", err)
	}

	pubData, err := base64.StdEncoding.DecodeString(saKey.PublicKey)
	if err != nil {
		return fmt.Errorf("failed to decode service-account.pub: %w", err)
	}
	keyData, err := base64.StdEncoding.DecodeString(saKey.PrivateKey)
	if err != nil {
		return fmt.Errorf("failed to decode service-account.key: %w", err)
	}

	if err := os.WriteFile(filepath.Join(saKeyDir, "service-account.pub"), pubData, 0400); err != nil {
		return fmt.Errorf("failed to write service-account.pub: %w", err)
	}
	if err := os.WriteFile(filepath.Join(saKeyDir, "service-account.key"), keyData, 0600); err != nil {
		return fmt.Errorf("failed to write service-account.key: %w", err)
	}

	klog.Info("Wrote shared service account signing keys")
	return nil
}
