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
// Original smali:
//
//	settings := engine.settings
//	if settings.allowNoKey { return true }
//	if settings.apiKey.isEmpty() { return false }
//	auth := session.headers["authorization"]            // lower-cased by NanoHTTPD
//	if !auth.startsWith("Bearer ", ignoreCase=true) { return false }
//	token := auth.substring(7)
//	return MessageDigest.isEqual(token.toByteArray(UTF_8), apiKey.toByteArray(UTF_8))
//
// The constant-time comparison is preserved via crypto/subtle.
//
// Note on paths: CPA applies frontend authentication to the whole proxy
// surface, whereas the app only guarded the AI routes and left /healthz and
// /authorize open. We mirror that by exempting the same two paths.
func frontendAuth(request []byte) ([]byte, error) {
	var req pluginapi.FrontendAuthRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	if isOpenPath(req.Path) {
		return okEnvelope(pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     "anonymous",
			Metadata:      map[string]string{"aigw_path": "public"},
		})
	}

	settings := state.settings.get()

	// V1/o.j(): allowNoKey short-circuits the whole check.
	if settings.AllowNoKey {
		return okEnvelope(pluginapi.FrontendAuthResponse{
			Authenticated: true,
			Principal:     principalFromHeaders(req.Headers),
			Metadata:      map[string]string{"aigw_auth": "allow_no_key"},
		})
	}

	// V1/o.j(): an empty configured key with allowNoKey=false can never pass.
	if settings.APIKey == "" {
		return denied("invalid_api_key",
			"网关已关闭「无 Key 调用」但尚未设置 API Key，请先配置 plugins.configs."+pluginName+".api_key")
	}

	raw := headerValue(req.Headers, "authorization")
	if raw == "" {
		return denied("invalid_api_key", "缺少或错误的 API Key")
	}

	// V1/o.j(): case-insensitive "Bearer " prefix, then substring(7).
	const bearer = "bearer "
	if len(raw) < len(bearer) || !strings.EqualFold(raw[:len(bearer)], bearer) {
		return denied("invalid_api_key", "缺少或错误的 API Key")
	}
	token := raw[len(bearer):]

	// MessageDigest.isEqual -> constant-time comparison.
	if subtle.ConstantTimeCompare([]byte(token), []byte(settings.APIKey)) != 1 {
		return denied("invalid_api_key", "缺少或错误的 API Key")
	}

	return okEnvelope(pluginapi.FrontendAuthResponse{
		Authenticated: true,
		Principal:     principalFromHeaders(req.Headers),
		Metadata:      map[string]string{"aigw_auth": "bearer"},
	})
}

// denied renders the authentication failure envelope.
//
// V1/o.j() returning false leads V1/o.k() to emit:
//
//	l(401, "invalid_api_key", msg)
//	-> {"error":{"message":msg,"type":"api_error","code":"invalid_api_key"}}
//
// CPA's frontend auth rejects with HTTP 401 authentication_error; the message
// still carries the ported wording so behaviour is recognisably the same.
func denied(code, message string) ([]byte, error) {
	return errorEnvelope(code, message, http.StatusUnauthorized), nil
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
	if v := headerValue(headers, "X-AIGW-Account"); v != "" {
		return v
	}
	if v := headerValue(headers, "X-Client-Id"); v != "" {
		return v
	}
	return "aigw-client"
}
