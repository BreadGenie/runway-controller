package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
	"github.com/BreadGenie/runway-controller/cache/store"
)

// check mode has no side effects. Initialization runs as part of the service,
// before cache-data starts; setup never generates the grant signing key.
func prepareCacheService(path string, check bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cache service configuration unavailable")
	}
	c, err := decodeCacheServiceConfig(raw)
	if err != nil {
		return errors.New("cache service configuration invalid (audience must be an HTTPS cache-data origin)")
	}
	origin, _ := url.Parse(c.Audience)
	if origin.Hostname() != "127.0.0.1" || origin.Port() == "" {
		return errors.New("release requires loopback cache-data audience")
	}
	if check {
		return nil
	}
	root := os.Getenv("RUNWAY_ROOT")
	local := func(path string) string { return filepath.Join(root, path) }
	addr, _ := netip.ParseAddrPort(c.Listen)
	for _, dir := range []string{filepath.Dir(c.SigningKeyFile), filepath.Dir(c.PrivateKeyFile), "/var/lib/runway-cache", "/var/lib/runway-cache/spools"} {
		if err := os.MkdirAll(local(dir), 0700); err != nil {
			return err
		}
	}
	key, err := ensureCacheServiceKey(local(c.SigningKeyFile))
	if err != nil {
		return err
	}
	if err := prepareCacheServiceTLS(c, local, addr.Addr().String()); err != nil {
		return err
	}
	public := key.Public().(ed25519.PublicKey)
	storage := os.Getenv("RUNWAY_STORAGE_CA_FILE")
	if storage == "" {
		storage = "/etc/runway/storage-ca.pem"
	}
	config := cacheDataServiceConfig{MetricsListen: c.DataMetricsListen, Version: 1, Listen: origin.Host, PublicOrigin: c.Audience, CertificateFile: c.CertificateFile, PrivateKeyFile: c.PrivateKeyFile, PublicKeys: map[string]string{grant.KeyID(public): hex.EncodeToString(public)}, RegionID: c.RegionID, StateDir: "/var/lib/runway-cache", QuotaBytes: 1 << 30, TotalBytes: 4 << 30, MaxArchiveBytes: c.ArchiveBytes, Disk: store.DiskConfig{Directory: "/var/lib/runway-cache/spools", PhysicalBytes: 8 << 30}, Managed: &cacheManagedConfig{CentralCAFile: c.CentralCAFile, CentralURL: c.CentralURL, CentralTokenFile: c.CentralTokenFile, StorageCAFile: storage}}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return releaseReplace(local("/etc/runway/cache-data.json"), data)
}

func releaseReplace(path string, data []byte) error {
	old, err := os.ReadFile(path)
	if err == nil && string(old) == string(data) {
		return nil
	}
	if err == nil {
		if err = os.WriteFile(path+".previous", old, 0600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.WriteFile(path+".new", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

func prepareCacheServiceTLS(c cacheServiceConfig, local func(string) string, domain string) error {
	// A self-signed local CA also serves as the service certificate. Preserve it
	// as a pair on upgrades; refuse partial state instead of replacing identity.
	_, certErr := os.Stat(local(c.CertificateFile))
	_, keyErr := os.Stat(local(c.PrivateKeyFile))
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		// The loopback data connection verifies the same certificate by IP.
		template.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		if ip := net.ParseIP(domain); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = []string{domain, "internal-" + domain}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, pub, private)
		if err != nil {
			return err
		}
		pk, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			return err
		}
		if err = os.WriteFile(local(c.PrivateKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0600); err != nil {
			return err
		}
		if err = os.WriteFile(local(c.CertificateFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			return err
		}
	} else if certErr != nil || keyErr != nil {
		return errors.New("incomplete TLS identity")
	}
	cert, err := os.ReadFile(local(c.CertificateFile))
	if err != nil {
		return err
	}
	private, err := os.ReadFile(local(c.PrivateKeyFile))
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, private)
	if err != nil {
		return errors.New("invalid TLS identity")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(domain) != nil || leaf.VerifyHostname("127.0.0.1") != nil {
		return errors.New("TLS identity does not cover service names")
	}
	if err = releaseReplace(local(c.CacheDataCAFile), cert); err != nil {
		return err
	}

	return nil
}
