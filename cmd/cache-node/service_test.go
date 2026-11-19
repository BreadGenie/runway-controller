package main

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceSetup(t *testing.T) {
	for _, outcome := range []string{"enabled", "pending-then-enabled", "pending-timeout", "disabled", "malformed", "timeout", "wrong-ca", "wrong-name", "plain-http", "redirect"} {
		t.Run(outcome, func(t *testing.T) {
			var attempts atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/setup" {
					t.Error("wrong setup endpoint")
				}
				var request struct {
					Version int
					Nonce   string
					Context map[string]*string
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Version != 2 {
					t.Error("wrong wire")
				}
				attempt := attempts.Add(1)
				switch outcome {
				case "pending-then-enabled", "pending-timeout":
					if attempt == 1 || outcome == "pending-timeout" {
						w.WriteHeader(http.StatusTooEarly)
						return
					}
					fmt.Fprintf(w, "{\"version\":2,\"nonce\":%q,\"outcome\":\"enabled\",\"route\":%q}\n", request.Nonce, route)
					return
				case "timeout":
					<-r.Context().Done()
					return
				case "malformed":
					fmt.Fprintln(w, `{}`)
					return
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1/setup", 302)
					return
				}
				fmt.Fprintf(w, "{\"version\":2,\"nonce\":%q,\"outcome\":%q,\"route\":%q}\n", request.Nonce, outcome, route)
			})
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			address := strings.TrimPrefix(server.URL, "https://")
			if outcome == "wrong-ca" {
				roots = x509.NewCertPool()
			}
			if outcome == "wrong-name" {
				address = "localhost:" + strings.Split(address, ":")[1]
			}
			if outcome == "plain-http" {
				plain := httptest.NewServer(handler)
				defer plain.Close()
				address = strings.TrimPrefix(plain.URL, "http://")
			}
			c := &serviceConfig{Service: address}
			deadline := time.Now().Add(time.Second)
			if outcome == "pending-then-enabled" {
				deadline = time.Now().Add(3 * time.Second)
			}
			if outcome == "timeout" || outcome == "pending-timeout" {
				deadline = time.Now().Add(60 * time.Millisecond)
			}
			wantEnabled := outcome == "enabled" || outcome == "pending-then-enabled"
			directory := t.TempDir()
			getenv := func(string) string { return "1" }
			if got := selectRoute(directory, getenv, func(time.Time) bool { return setupServiceWithRoots(c, roots, deadline, getenv) }); got != wantEnabled {
				t.Fatalf("enabled=%v", got)
			}
			if got := selectRoute(directory, getenv, func(time.Time) bool { t.Fatal("persisted decision retried setup"); return false }); got != wantEnabled {
				t.Fatal("decision not persisted")
			}
		})
	}
}

func TestServiceConfig(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"version":1,"mode":"service","service":"http://internal-cache:443","ca_file":"/etc/runway/cache-ca.pem"}`,
		`{"version":1,"mode":"service","service":"internal-cache:0","ca_file":"/etc/runway/cache-ca.pem"}`,
		`{"version":1,"mode":"service","service":"[2001:db8::7]:8443","ca_file":"/etc/runway/cache-ca.pem"}`,
		`{"version":1,"mode":"service","service":"internal-cache:65536","ca_file":"/etc/runway/cache-ca.pem"}`,
		`{"version":1,"mode":"service","service":"internal-cache:443","ca_file":"/tmp/ca"}`,
		`{"version":1,"mode":"vsock","extra":1}`,
	} {
		if _, err := decodeServiceConfig([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	c, err := decodeServiceConfig([]byte(`{"version":1,"mode":"service","service":"internal-cache:443","ca_file":"/etc/runway/cache-ca.pem"}`))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	c, err = decodeServiceConfig([]byte(`{"version":1,"mode":"service","service":"[fdaa:1::7]:8443","ca_file":"/etc/runway/cache-ca.pem"}`))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	c, err = decodeServiceConfig([]byte(`{"version":1,"mode":"vsock"}`))
	if err != nil || c != nil {
		t.Fatal(c, err)
	}
	path := filepath.Join(t.TempDir(), "config")
	if c, err := readServiceConfig(path); c != nil || err != nil {
		t.Fatal("legacy missing config", err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if _, err := readServiceConfig(path); err == nil {
		t.Fatal("followed symlink")
	}
}
