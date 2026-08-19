// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reverseproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/caddyserver/caddy/v2"
)

// TestActiveHealthCheckRequestBody ensures a literal active health-check body
// (e.g. JSON) is sent verbatim. Regression for #7021: the body was expanded
// with ReplaceAll, which treats an unrecognized "{...}" as a placeholder and
// replaces it with the empty string, so the request body arrived empty.
func TestActiveHealthCheckRequestBody(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`

	got := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	h := &Handler{
		ctx: ctx,
		HealthChecks: &HealthChecks{
			Active: &ActiveHealthChecks{
				Body:   body,
				Method: http.MethodGet,
				Path:   "/",
				// Keep passes below the healthy threshold so the health check
				// does not flip host state and emit an event (h.events is nil).
				Passes:     2,
				httpClient: backend.Client(),
				logger:     zap.NewNop(),
			},
		},
	}

	addr := strings.TrimPrefix(backend.URL, "http://")
	upstream := &Upstream{Host: new(Host), Dial: addr}

	if err := h.doActiveHealthCheck(DialInfo{}, addr, addr, upstream); err != nil {
		t.Fatalf("doActiveHealthCheck: %v", err)
	}

	select {
	case b := <-got:
		if b != body {
			t.Errorf("active health check sent body %q, want %q", b, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not receive the health check request")
	}
}
