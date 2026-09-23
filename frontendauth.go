package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// frontendAuth is the port of V1/o.j(Session) from AI 聚合网关 0.1.18.
//
// # IMPORTANT — how this differs from the source gateway
//
// The APK was a standalone gateway, so its bearer check was the only gate in
// front of the proxy. CPA is different: every CPA deployment already has its own
// front-end authentication (the `api-keys` list, plus any other access
// providers) applied to /v1/* before a request reaches a provider. Registering a
// second mandatory gate here means an operator's existing CPA keys stop working
// the moment this plugin is enabled — the failure reads
// `{"error":"Missing API key"}` with a 401, even though the key is perfectly
// valid for CPA.
//
// The capability therefore behaves as an *additional* accepted credential rather
// than a replacement:
//
//   - disabled by default (EnforceFrontendKey = false): the plugin never rejects
//     anything, so CPA's own authentication decides
//   - when enabled: a request carrying the configured api_key is accepted here;
//     anything else is reported as unauthenticated, which CPA maps to
//     "not handled" and passes on to the other providers
//
// That keeps the source gateway's behaviour available for a locked-down
// deployment without breaking the host's own auth.
func frontendAuth(request []byte) ([]byte, error) {
	var req pluginapi.FrontendAuthRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	settings := state.settings.get()

	// Not enforcing: defer entirely to CPA's own authentication.
	//
	// Note this also covers the source app's allowNoKey=true case, which is why
	// there is no separate branch for it below.
	if !settings.EnforceFrontendKey {
		return okEnvelope(pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     principalFromHeaders(req.Headers),
			Metadata: map[string]string{
				"workbuddy_auth": "delegated",
			},
		})
	}

	// Enforcing but no key configured: nothing to compare against, so defer
	// rather than lock everyone out.
	if settings.APIKey == "" {
		return okEnvelope(pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     principalFromHeaders(req.Headers),
			Metadata:      map[string]string{"workbuddy_auth": "no_key_configured"},
		})
	}

	// When enforcement is on, the source app's allowNoKey still short-circuits.
	if settings.AllowNoKey {
		return okEnvelope(pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     principalFromHeaders(req.Headers),
			Metadata:      map[string]string{"workbuddy_auth": "allow_no_key"},
		})
	}

	raw := headerValue(req.Headers, "authorization")
	if raw == "" {
		return unauthenticated("缺少或错误的 API Key")
	}

	// V1/o.j(): case-insensitive "Bearer " prefix, then substring(7).
	const bearer = "bearer "
	if len(raw) < len(bearer) || !strings.EqualFold(raw[:len(bearer)], bearer) {
		return unauthenticated("缺少或错误的 API Key")
	}
	token := raw[len(bearer):]

	// MessageDigest.isEqual -> constant-time comparison.
	if subtle.ConstantTimeCompare([]byte(token), []byte(settings.APIKey)) != 1 {
		return unauthenticated("缺少或错误的 API Key")
	}

	return okEnvelope(pluginapi.FrontendAuthResponse{
		Authenticated: true,
		Principal:     principalFromHeaders(req.Headers),
		Metadata:      map[string]string{"workbuddy_auth": "bearer"},
	})
}

// unauthenticated reports a request this provider declined to authenticate.
//
// CPA maps `Authenticated:false` to sdkaccess.NotHandledError
// (internal/pluginhost/adapters_auth.go:135), so the other access providers —
// including CPA's own api-keys list — still get their chance. Returning a hard
// 401 here would veto the whole chain.
func unauthenticated(message string) ([]byte, error) {
	return okEnvelope(pluginapi.FrontendAuthResponse{
		Authenticated: false,
		Metadata:      map[string]string{"workbuddy_error": message},
	})
}

// isOpenPath mirrors the unauthenticated routes of V1/o.e(Session):
// GET /healthz and GET /authorize bypass V1/o.j().
func isOpenPath(path string) bool {
	p := strings.TrimSuffix(strings.TrimSpace(path), "/")
	switch p {
	case "/healthz", "/authorize", "":
		return true
	}
	// /v1/models is authenticated by V1/o.n() in the app, so it is NOT open.
	return false
}

func headerValue(headers http.Header, key string) string {
	if headers == nil {
		return ""
	}
	// http.Header.Get is already case-insensitive.
	if v := headers.Get(key); v != "" {
		return v
	}
	// Defensive: CPA may ship a non-canonical map.
	for k, vs := range headers {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// principalFromHeaders names the caller for CPA's request log, preferring the
// client-supplied identifiers the app also surfaced.
func principalFromHeaders(headers http.Header) string {
	if v := headerValue(headers, "X-WorkBuddy-Account"); v != "" {
		return v
	}
	if v := headerValue(headers, "X-Client-Id"); v != "" {
		return v
	}
	return "workbuddy-client"
}
