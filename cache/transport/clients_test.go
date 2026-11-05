package transport

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/store"
	"github.com/BreadGenie/runway/cache/store/storetest"
)

func TestPinnedClientsThroughRelay(t *testing.T) {
	if os.Getenv("RUNWAY_CACHE_CLIENT_TEST") != "1" {
		t.Skip("set RUNWAY_CACHE_CLIENT_TEST=1 for offline pinned Node/Yarn integration")
	}
	service := storetest.Start(t, func(c *store.Config) {
		c.QuotaBytes, c.TotalBytes, c.MaxArchiveBytes, c.Disk.PhysicalBytes = 8<<20, 8<<20, 4<<20, 32<<20
	})
	bearer := service.Signer.Writer(t)
	var downloads, chunks atomic.Int32
	inner := service.HTTP.Config.Handler
	service.HTTP.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			chunks.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/download/") {
			downloads.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("ticket read carried bearer")
			}
		} else if r.Header.Get("Authorization") != "Bearer "+bearer {
			t.Error("control request lost host grant")
		}
		inner.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/download/") {
			size, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
			if err != nil || size <= 64<<10 {
				t.Error("client archive did not exceed the control response bound")
			}
		}
	})
	_, front := storeFront(t, service, bearer, func(c *Config) {
		c.Limits.RequestBytes, c.Limits.ResponseBytes, c.Limits.ChunkBytes, c.Limits.ArchiveBytes = 64<<10, 64<<10, 4<<20, 4<<20
		c.Limits.Concurrent, c.Limits.Timeout = 2, 20*time.Second
	})
	origin, _ := url.Parse(front.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "../../integrations/actions-cache/test_relay_clients.py", "--transport-port", origin.Port())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned relay client regression failed: %v\n%s", err, output)
	}
	if downloads.Load() != 3 {
		t.Fatalf("relay archive reads=%d, want 3", downloads.Load())
	}
	if chunks.Load() < 8 {
		t.Fatalf("upload chunks=%d, want at least 8", chunks.Load())
	}
	t.Log(string(output))
}
