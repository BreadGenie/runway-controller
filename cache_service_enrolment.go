package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"golang.org/x/sys/unix"
)

func cacheCentralClient(c cacheServiceConfig) (*http.Client, error) {
	var roots *x509.CertPool
	if c.CentralCAFile != "" {
		data, err := readProtectedCacheFile(c.CentralCAFile, false)
		if err != nil {
			return nil, err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, grant.ErrInvalid
		}
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func cacheServiceMeshAddress(ip netip.Addr, region uint16) bool {
	if !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
		return false
	}
	b := ip.As16()
	return b[0] == 0xfd && b[1] == 0xaa && binary.BigEndian.Uint16(b[2:4]) == region && binary.BigEndian.Uint32(b[4:8]) == 0
}

func writeCacheCredential(path, token string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".cache-credential-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString(token); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type cacheSiteLeaseClient struct {
	site       cacheCentralHTTPS
	credential func() (string, error)
}

func (c cacheSiteLeaseClient) lease(ctx context.Context, address netip.Addr) (cacheServiceLease, error) {
	token, err := c.credential()
	if err != nil {
		return cacheServiceLease{}, grant.ErrInvalid
	}
	site := c.site
	site.token = token
	return (cacheCentralWithBuckets{site}).lease(ctx, address)
}

func validCacheCredential(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && len(value) == 43
}

func readCacheCredential(path string) (string, error) {
	raw, err := readProtectedCacheFile(path, true)
	token := strings.TrimSpace(string(raw))
	if err != nil || !validCacheCredential(token) {
		return "", grant.ErrInvalid
	}
	return token, nil
}

var errCacheCredentialRejected = errors.New("Runway site rejected cache credential")

func (site cacheCentralHTTPS) exchange(ctx context.Context, method string, payload map[string]any) error {
	if !cacheHTTPSOrigin(site.origin) || site.client == nil {
		return grant.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return grant.ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(site.origin, "/")+"/api/method/runway.api."+method, bytes.NewReader(body))
	if err != nil {
		return grant.ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	if site.token != "" {
		req.Header.Set("Authorization", "token "+site.token)
	}
	client := *site.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Runway cache credential exchange unavailable")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errCacheCredentialRejected
	case http.StatusOK:
	default:
		return errors.New("Runway cache credential exchange failed")
	}
	var answer struct {
		Credential string `json:"credential"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &answer) != nil || answer.Credential != payload["credential"] {
		return errors.New("Runway cache credential response invalid")
	}
	return nil
}

func manageCacheCredential(path string, rotate bool) error {
	raw, err := readProtectedCacheFile(path, true)
	if err != nil {
		return err
	}
	c, err := decodeCacheServiceConfig(raw)
	if err != nil {
		return err
	}
	if checkCachePrivateDirectory(filepath.Dir(c.CentralTokenFile)) != nil {
		return grant.ErrInvalid
	}
	client, err := cacheCentralClient(c)
	if err != nil {
		return err
	}
	return exchangeCacheCredential(context.Background(), c, client, rotate, func() (string, error) {
		return readCacheCredential("/etc/runway/enrolment-token")
	})
}

// Keep the candidate before sending it. After an interrupted reply or local
// write, authenticating with the candidate establishes whether it was accepted.
func exchangeCacheCredential(ctx context.Context, c cacheServiceConfig, client *http.Client, rotate bool, bootstrap func() (string, error)) error {
	path := c.CentralTokenFile
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	token := strings.TrimSpace(string(current))
	if len(current) > 4096 || token != "" && !validCacheCredential(token) {
		return grant.ErrInvalid
	}
	if !rotate && token != "" {
		return nil
	}
	if rotate && token == "" {
		return errCacheCredentialRejected
	}
	pending := path + ".pending"
	raw, err := os.ReadFile(pending)
	candidate := string(raw)
	recovering := err == nil
	if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		candidate = base64.RawURLEncoding.EncodeToString(secret[:])
		if err := writeCacheCredential(pending, candidate); err != nil {
			return err
		}
	} else if err != nil || !validCacheCredential(candidate) {
		return grant.ErrInvalid
	}
	site := cacheCentralHTTPS{origin: c.CentralURL, client: client, token: candidate}
	payload := map[string]any{"credential": candidate}
	if recovering {
		err = site.exchange(ctx, "cache_rotate", payload)
	}
	if !recovering || errors.Is(err, errCacheCredentialRejected) {
		site.token = token
		method := "cache_rotate"
		if !rotate {
			oneUse, err := bootstrap()
			if err != nil {
				return errors.New("cache enrolment token unavailable")
			}
			ca, err := os.ReadFile(c.CacheDataCAFile)
			if err != nil {
				return errors.New("cache enrolment CA unavailable")
			}
			addr, err := netip.ParseAddrPort(c.Listen)
			if err != nil {
				return grant.ErrInvalid
			}
			method = "cache_enrol"
			payload["region"], payload["token"], payload["address"], payload["port"], payload["ca"] =
				c.RegionID, oneUse, addr.Addr().String(), addr.Port(), string(ca)
		}
		err = site.exchange(ctx, method, payload)
	}
	if err != nil {
		return err
	}
	if err := writeCacheCredential(path, candidate); err != nil {
		return err
	}
	return os.Remove(pending)
}
