package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// interceptRequest ports the request-rewriting half of V1/o.k().
//
// Runs on both hooks CPA offers, mirroring the two phases of the original:
//
//	InterceptRequestBeforeAuth -> resolve provider + rewrite body.model
//	                              (the app resolved the route before touching
//	                              any credential, V1/o.k steps 6-8)
//	InterceptRequestAfterAuth  -> stamp the selected credential and enforce
//	                              default_provider (V1/k.b, engine default)
//
// The App's account-pool rotation loop (step 9) lives in the *executor*, not
// the interceptor; CPA's own retry/auth-rotation machinery covers it, and the
// cooldown policy is applied from the response hooks via state.pool.
func interceptRequest(request []byte, afterAuth bool) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	// Only chat-completions style payloads carry "model"; everything else is
	// passed through untouched, exactly like the app's unknown-route 404 path.
	if len(req.Body) == 0 {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	meta, okMeta := parseRequestMeta(req.Body)
	if !okMeta {
		// V1/o.k(): "请求体不是合法 JSON" -> 400 invalid_request.
		// CPA will surface its own parse error; do not fight it.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	settings := state.settings.get()
	providers := knownProviders(req.Metadata)

	requested := strings.TrimSpace(meta.Model)
	if requested == "" {
		requested = strings.TrimSpace(req.RequestedModel)
	}

	res, okRoute := resolveRoute(requested, settings, providers)

	// V1/o.k(): "缺少 model 参数" -> 400 invalid_request.
	if !okRoute {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "缺少 model 参数",
				"type":    "invalid_request_error",
				"code":    "invalid_request",
			},
		})
		return okEnvelope(pluginapi.RequestInterceptResponse{
			Terminate:       true,
			StatusCode:      http.StatusBadRequest,
			ResponseHeaders: jsonHeaders(),
			ResponseBody:    body,
		})
	}

	// enforce_default_provider: an opt-in strictness knob the original app did
	// not have (it always honoured an explicit prefix).
	if settings.EnforceDefaultProvider && res.Provider != settings.DefaultProvider {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "该网关仅允许使用默认供应商：" + settings.DefaultProvider,
				"type":    "invalid_request_error",
				"code":    "provider_not_allowed",
			},
		})
		return okEnvelope(pluginapi.RequestInterceptResponse{
			Terminate:       true,
			StatusCode:      http.StatusBadRequest,
			ResponseHeaders: jsonHeaders(),
			ResponseBody:    body,
		})
	}

	if afterAuth {
		// Record which credential CPA selected so the response hooks can apply
		// the app's per-account cooldown policy.
		if req.Metadata != nil {
			if uid := metadataString(req.Metadata, "auth_id"); uid != "" {
				label := metadataString(req.Metadata, "auth_label")
				state.pool.observe(res.Provider, uid, label)
			}
		}
		// The body was already rewritten before auth; nothing left to do.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	resp := pluginapi.RequestInterceptResponse{}

	// V1/o.k() step 8: rewrite body.model with the provider mapping and, when
	// the client used the explicit "provider/model" form, collapse it to the
	// bare upstream model name.
	target := res.Model
	if rewritten, changed := rewriteModelBody(req.Body, target); changed {
		resp.Body = rewritten
		res.Rewritten = true
	} else if requested != target && strings.Contains(requested, "/") {
		// The body already carried the mapped name but still holds a prefix;
		// force the strip so upstream sees a bare model id.
		if rewritten, changed := rewriteModelBody(req.Body, target); changed {
			resp.Body = rewritten
			res.Rewritten = true
		}
	}

	// Propagate routing hints for the executor / response hooks via headers so
	// they survive across the interceptor chain and show up in request logs.
	if resp.Headers == nil {
		resp.Headers = http.Header{}
	}
	resp.Headers.Set("X-WorkBuddy-Provider", res.Provider)
	resp.Headers.Set("X-WorkBuddy-Model", target)
	resp.Headers.Set("X-WorkBuddy-Requested-Model", requested)
	if res.Explicit {
		resp.Headers.Set("X-WorkBuddy-Route", "explicit")
	} else {
		resp.Headers.Set("X-WorkBuddy-Route", "default")
	}

	return okEnvelope(resp)
}

func jsonHeaders() http.Header {
	return http.Header{
		"Content-Type":                []string{"application/json; charset=utf-8"},
		"Access-Control-Allow-Origin": []string{"*"},
	}
}

// knownProviders extracts the pipeline's available provider keys.
//
// CPA hands plugins a best-effort metadata snapshot; ModelRouteRequest gives a
// first-class list, and RequestInterceptRequest.Metadata may carry provider
// hints. When nothing is discoverable the resolver falls back to the single
// configured default_provider, matching the app's behaviour on a fresh install
// where only one provider is authenticated.
func knownProviders(meta map[string]any) []string {
	var out []string
	seen := map[string]struct{}{}

	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}

	for _, key := range []string{"providers", "available_providers", "provider_ids"} {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if s, okString := item.(string); okString {
					add(s)
				}
			}
		case []string:
			for _, s := range v {
				add(s)
			}
		}
	}

	// The configured default is always a valid target.
	add(state.settings.get().DefaultProvider)
	return out
}

func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key]; ok {
		if s, okString := v.(string); okString {
			return s
		}
	}
	return ""
}

// metadataInt coerces the loosely-typed metadata bag to an int.
func metadataInt(meta map[string]any, key string) (int64, bool) {
	if meta == nil {
		return 0, false
	}
	raw, ok := meta[key]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, errInt := v.Int64()
		return n, errInt == nil
	}
	return 0, false
}

// requestContext carries state from the request hooks to the response hooks.
// CPA correlates calls by RequestID, so the plugin keeps a bounded map.
type requestContext struct {
	Provider       string
	Model          string
	RequestedModel string
	UID            string
	Label          string
	Stream         bool
	StartedAt      time.Time
	// Variant is the supplier realm of the account that served this request
	// ("cn" / "ai"). The call log needs it because Provider is a constant
	// ("codebuddy") for both realms, so a mixed pool produced records that could
	// not be told apart.
	Variant string
}

var inflight = newInflightMap()

// inflightMap is a small, mutex-guarded RequestID -> requestContext map with a
// hard size cap so a misbehaving upstream cannot grow it without bound.
type inflightMap struct {
	mu    sync.Mutex
	items map[string]requestContext
	order []string
	max   int
}

func newInflightMap() *inflightMap {
	return &inflightMap{items: make(map[string]requestContext), max: 4096}
}

func (m *inflightMap) put(id string, ctx requestContext) {
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.items[id]; !exists {
		m.order = append(m.order, id)
	}
	m.items[id] = ctx
	for len(m.order) > m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.items, oldest)
	}
}

func (m *inflightMap) get(id string) (requestContext, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	return v, ok
}

func (m *inflightMap) take(id string) (requestContext, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if ok {
		delete(m.items, id)
	}
	return v, ok
}
