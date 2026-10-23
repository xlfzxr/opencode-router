package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode-router/internal/config"
	"opencode-router/internal/egress"
)

func TestCatalogIntersectionRefreshAndFailure(t *testing.T) {
	var version atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inference/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"new-model"},{"id":"paid-model"},{"id":"next-model"}]}`)
		case "/catalog":
			switch version.Load() {
			case 0:
				fmt.Fprint(w, `{"opencode":{"models":{"new-model":{"cost":{"input":0,"output":0}},"paid-model":{"cost":{"input":2,"output":2}},"unavailable-model":{"cost":{"input":0,"output":0}}}}}`)
			case 1:
				fmt.Fprint(w, `{"opencode":{"models":{"next-model":{"cost":{"input":0,"output":0}}}}}`)
			default:
				w.WriteHeader(503)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	e := egress.New(config.Proxy{}, time.Second)
	defer e.CloseIdle()
	client := &Client{BaseURL: server.URL, CatalogURL: server.URL + "/catalog", Egress: e, Sessions: NewSessionStore(time.Hour, 10), RequestTimeout: time.Second, CatalogTTL: 0}
	for i, want := range []string{"new-model", "next-model"} {
		version.Store(int32(i))
		models, err := client.ListModels(context.Background(), CallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 || models[0].ID != want {
			t.Fatalf("discovery %d: %+v", i, models)
		}
	}
	// No stale or hardcoded fallback after refresh fails.
	version.Store(2)
	if models, err := client.ListModels(context.Background(), CallOptions{}); err == nil || len(models) > 0 {
		t.Fatalf("failed catalog returned models: %+v %v", models, err)
	}
}
