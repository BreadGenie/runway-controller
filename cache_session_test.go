package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
)

// Observe dependencies rather than session state. Both real authority adapters
// exercise the same lifecycle interface against a real TLS translator target.
type sessionAuthorityProbe struct {
	cacheJobAuthority
	resolveHook    func(context.Context)
	authorizeHook  func(context.Context, *cacheJobGrant)
	resolutions    atomic.Int32
	authorizations atomic.Int32
}

func (a *sessionAuthorityProbe) resolve(ctx context.Context, b cacheGuestBinding, hint int64) (cacheJobRelease, error) {
	a.resolutions.Add(1)
	if a.resolveHook != nil {
		a.resolveHook(ctx)
	}
	return a.cacheJobAuthority.resolve(ctx, b, hint)
}
func (a *sessionAuthorityProbe) authorize(ctx context.Context, b cacheGuestBinding, r cacheJobRelease, request grant.SetupRequest) (cacheJobGrant, error) {
	a.authorizations.Add(1)
	verified, err := a.cacheJobAuthority.authorize(ctx, b, r, request)
	if err == nil && a.authorizeHook != nil {
		a.authorizeHook(ctx, &verified)
	}
	return verified, err
}

type sessionFixture struct {
	session      *cacheSession
	authority    *sessionAuthorityProbe
	test         *authorityCase
	authenticate func() (cacheGuestBinding, string)
	bearers      chan string
}

func newSessionFixture(t *testing.T, managed bool, configure func(*cacheSessionConfig, *sessionAuthorityProbe), parents ...context.Context) *sessionFixture {
	t.Helper()
	s, test := serviceFixture(t, nil, http.StatusOK, false)
	config := s.template
	var authority cacheJobAuthority
	binding := test.binding
	if managed {
		authority, binding = serviceAuthorityFixture(t, s)
		config.id, config.deadline = "lease-1", binding.ExpiresAt
		config.nonceLimit, config.bindTickets = 4096, true
	} else {
		github, _ := test.authority(t)
		authority = &github
		config.id = binding.VMID
	}
	probe := &sessionAuthorityProbe{cacheJobAuthority: authority}
	config.authority = probe
	bearers := make(chan string, 16)
	data := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case bearers <- r.Header.Get("Authorization"):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(data.Close)
	config.audience = data.URL
	config.roots = data.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if configure != nil {
		configure(&config, probe)
	}
	parent := t.Context()
	if len(parents) > 0 {
		parent = parents[0]
	}
	session := newCacheSession(parent, config)
	t.Cleanup(session.close)
	return &sessionFixture{session: session, authority: probe, test: test, bearers: bearers, authenticate: func() (cacheGuestBinding, string) { return binding, "" }}
}

func (f *sessionFixture) enable(ctx context.Context, request grant.SetupRequest) string {
	return f.session.enable(ctx, request, f.authenticate)
}
func (f *sessionFixture) lookup() int {
	w := httptest.NewRecorder()
	f.session.translate(w, httptest.NewRequest(http.MethodGet, "/_apis/artifactcache/cache?keys=yarn&version="+strings.Repeat("a", 64), nil), "")
	return w.Code
}
func sessionModes(t *testing.T, test func(*testing.T, bool)) {
	t.Helper()
	for _, mode := range []struct {
		name    string
		managed bool
	}{{"standalone", false}, {"managed", true}} {
		t.Run(mode.name, func(t *testing.T) { test(t, mode.managed) })
	}
}
func awaitSessionSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("session operation did not arrive")
	}
}

func TestCacheSessionOneShotAndReplay(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		f := newSessionFixture(t, managed, nil)
		if got := f.lookup(); got != 503 {
			t.Fatal("unconfigured translator", got)
		}
		wrong := f.test.setup
		sha := strings.Repeat("a", 40)
		wrong.Context.SHA = &sha
		if got := f.enable(t.Context(), wrong); got == "" {
			t.Fatal("mismatched evidence accepted")
		}
		if got := f.enable(t.Context(), wrong); got != "nonce_replayed" {
			t.Fatal(got)
		}
		request := f.test.setup
		request.Nonce = "8b9c0d1e-2f3a-4b4c-8d5e-6f7a8b9c0d1e"
		if got := f.enable(t.Context(), request); got != "" {
			t.Fatal(got)
		}
		if got := f.lookup(); got != 204 {
			t.Fatal(got)
		}
		if got := f.enable(t.Context(), request); got != "already_enabled" {
			t.Fatal(got)
		}
		if f.authority.resolutions.Load() != 1 {
			t.Fatal("job evidence was not pinned")
		}
	})
}

func TestCacheSessionConcurrentSetup(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		entered, proceed := make(chan struct{}), make(chan struct{})
		f := newSessionFixture(t, managed, func(_ *cacheSessionConfig, a *sessionAuthorityProbe) {
			a.resolveHook = func(ctx context.Context) {
				close(entered)
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			}
		})
		authenticated := make(chan struct{}, 4)
		authenticate := f.authenticate
		f.authenticate = func() (cacheGuestBinding, string) { authenticated <- struct{}{}; return authenticate() }
		results := make(chan string, 2)
		go func() { results <- f.enable(t.Context(), f.test.setup) }()
		awaitSessionSignal(t, entered)
		request := f.test.setup
		request.Nonce = "8b9c0d1e-2f3a-4b4c-8d5e-6f7a8b9c0d1e"
		go func() { results <- f.enable(t.Context(), request) }()
		awaitSessionSignal(t, authenticated)
		awaitSessionSignal(t, authenticated)
		close(proceed)
		var enabled int
		for range 2 {
			select {
			case got := <-results:
				if got == "" {
					enabled++
				} else if got != "already_enabled" {
					t.Fatal(got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("setup blocked")
			}
		}
		if enabled != 1 || f.authority.resolutions.Load() != 1 {
			t.Fatal("concurrent setup did not share evidence", enabled)
		}
	})
}

func TestCacheSessionRevalidatesBeforeAcceptance(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		f := newSessionFixture(t, managed, nil)
		authenticate := f.authenticate
		var calls int
		f.authenticate = func() (cacheGuestBinding, string) {
			calls++
			if calls > 1 {
				return cacheGuestBinding{}, "unbound"
			}
			return authenticate()
		}
		if got := f.enable(t.Context(), f.test.setup); got != "unbound" {
			t.Fatal(got)
		}
		if f.session.enabled() || f.lookup() != 503 {
			t.Fatal("removed binding installed translator")
		}
	})
}

func TestCacheSessionClosureAndExpiry(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		for _, stop := range []string{"close", "parent", "deadline"} {
			t.Run(stop, func(t *testing.T) {
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				f := newSessionFixture(t, managed, func(c *cacheSessionConfig, _ *sessionAuthorityProbe) {
					if stop == "deadline" {
						c.deadline = time.Now().Add(150 * time.Millisecond)
					}
				}, parent)
				if got := f.enable(t.Context(), f.test.setup); got != "" {
					t.Fatal(got)
				}
				if stop == "parent" {
					cancel()
				}
				if stop == "close" {
					f.session.close()
					f.session.close()
				}
				for deadline := time.Now().Add(time.Second); !f.session.done(); {
					if time.Now().After(deadline) {
						t.Fatal("session did not close")
					}
					time.Sleep(time.Millisecond)
				}
				if f.lookup() != 503 {
					t.Fatal("closed translator remained available")
				}
				if got := f.enable(t.Context(), f.test.setup); got != "binding_expired" {
					t.Fatal(got)
				}
			})
		}
	})
}

func TestCacheSessionCloseCancelsSetup(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		entered, cancelled := make(chan struct{}), make(chan struct{})
		f := newSessionFixture(t, managed, func(_ *cacheSessionConfig, a *sessionAuthorityProbe) {
			a.resolveHook = func(ctx context.Context) { close(entered); <-ctx.Done(); close(cancelled) }
		})
		result := make(chan string, 1)
		go func() { result <- f.enable(t.Context(), f.test.setup) }()
		awaitSessionSignal(t, entered)
		f.session.close()
		awaitSessionSignal(t, cancelled)
		select {
		case got := <-result:
			if got == "" {
				t.Fatal("closed setup accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("setup outlived closure")
		}
		if f.session.enabled() || f.lookup() != 503 {
			t.Fatal("closed session published setup")
		}
	})
}

func TestCacheSessionRenewalKeepsTranslatorAvailable(t *testing.T) {
	sessionModes(t, func(t *testing.T, managed bool) {
		entered, proceed := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		f := newSessionFixture(t, managed, func(c *cacheSessionConfig, a *sessionAuthorityProbe) {
			c.renewWindow = 2 * time.Second
			a.authorizeHook = func(ctx context.Context, verified *cacheJobGrant) {
				switch calls.Add(1) {
				case 1:
					verified.Authorization.Deadline = time.Now().Add(8 * time.Second)
				case 2:
					close(entered)
					select {
					case <-proceed:
					case <-ctx.Done():
					}
					verified.Authorization.Deadline = time.Now().Add(time.Minute)
				}
			}
		})
		if got := f.enable(t.Context(), f.test.setup); got != "" {
			t.Fatal(got)
		}
		if f.lookup() != 204 {
			t.Fatal("initial grant unavailable")
		}
		initial := <-f.bearers
		awaitSessionSignal(t, entered)
		if f.lookup() != 204 {
			t.Fatal("renewal blocked translation")
		}
		<-f.bearers
		close(proceed)
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if f.lookup() != 204 {
				t.Fatal("replacement unavailable")
			}
			if bearer := <-f.bearers; bearer != initial {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("renewed grant was not installed")
			}
		}
	})
}
