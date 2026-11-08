package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The SDK does not expose a REST client. Use the same installation and key,
// caching its installation token, with no automatic HTTP retries.
func newControllerAppREST(appID string, installation int64, pem []byte) (*http.Client, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, errors.New("invalid GitHub App key")
	}
	var mu sync.Mutex
	var token string
	var expires time.Time
	transport := controllerRESTTransport(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Until(expires) < time.Minute {
			now := time.Now()
			signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: appID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}).SignedString(key)
			if err != nil {
				return nil, errCacheRepository
			}
			r, err := http.NewRequestWithContext(req.Context(), http.MethodPost, "https://api.github.com/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens", nil)
			if err != nil {
				return nil, err
			}
			r.Header.Set("Authorization", "Bearer "+signed)
			r.Header.Set("Accept", "application/vnd.github+json")
			response, err := http.DefaultTransport.RoundTrip(r)
			if err != nil {
				return nil, errCacheRepository
			}
			defer response.Body.Close()
			var result struct {
				Token     string    `json:"token"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if response.StatusCode != http.StatusCreated || json.NewDecoder(http.MaxBytesReader(nil, response.Body, 65536)).Decode(&result) != nil || result.Token == "" || !result.ExpiresAt.After(now) {
				return nil, errCacheRepository
			}
			token, expires = result.Token, result.ExpiresAt
		}
		return (cacheRepositoryCredential{token: token, transport: http.DefaultTransport}).RoundTrip(req)
	})
	return &http.Client{Transport: transport}, nil
}

type controllerRESTTransport func(*http.Request) (*http.Response, error)

func (f controllerRESTTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
