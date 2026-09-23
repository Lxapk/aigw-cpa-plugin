package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file ports WorkBuddy / CodeBuddy (Tencent) authentication from
// AI 聚合网关 0.1.18.
//
// Source evidence (JADX):
//
//	a2/b.java:313  a()  -> provider key is "codebuddy"   (NOT "workbuddy")
//	a2/b.java:681  n()  -> display name "WorkBuddy"
//	a2/b.java:61   f4226c = Y1.b.f3993e                   (DEVICE_CODE login)
//	Y1/b.java      enum: WEBVIEW_CALLBACK=0, DEVICE_CODE=1, SMS_CODE=2,
//	                     OAUTH_LOOPBACK=3, NONE=4
//
// Because WorkBuddy uses DEVICE_CODE rather than an OAuth loopback, the flow
// works from a remote server with no public callback URL:
//
//  1. POST https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI
//     -> {"code":0,"data":{"state":"<uuid>","authUrl":"https://..."}}
//  2. user opens authUrl and signs in
//  3. GET  https://copilot.tencent.com/v2/plugin/auth/token?state=<state>
//     -> {"code":11217}                      still waiting
//     -> {"code":0,"data":{...credentials}}  success
//     -> {"code":<other>,"msg":"..."}        failure
//
// Smali / source anchors:
//
//	V1/k.java:378  state request
//	N1/B.java:55   poll request; l3==11217 -> keep polling, l3!=0 -> error
//	a2/b.java:t()  credential field aliases
//	a2/b.java:E()  persisted credential JSON shape
//	a2/b.java:p()  upstream request headers
const (
	workBuddyProviderKey = "codebuddy"
	workBuddyDisplayName = "WorkBuddy"

	// copilotHostDefault is Tencent's CLI auth host, used for both the state and
	// token endpoints (V1/k.java:378 and N1/B.java:55). It is a variable so
	// tests can point the flow at a local server.
	copilotHostDefault = "https://copilot.tencent.com"

	// workBuddyGlobalDefault is the international WorkBuddy API host (a2/b.q()).
	workBuddyGlobalDefault = "https://www.workbuddy.ai"

	// codebuddyUA mirrors a2/b.java's "CLI/2.63.2 CodeBuddy/2.63.2".
	codebuddyUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// workBuddyLoginCodePending is the "still waiting" sentinel from N1/B.java.
	workBuddyLoginCodePending = 11217

	// workBuddyGlobalDomain selects the international endpoints.
	workBuddyGlobalDomain = "global"

	// workBuddyDefaultRegion mirrors V1/i.java case 9: region defaults to "cn".
	workBuddyDefaultRegion = "cn"
)

// copilotHost is the live Tencent CLI auth base URL. Kept as a package variable
// (guarded by copilotHostMu) so tests can redirect it; production reads the
// default.
var (
	copilotHostMu sync.RWMutex
	copilotHost   = copilotHostDefault
)

func copilotHostValue() string {
	copilotHostMu.RLock()
	defer copilotHostMu.RUnlock()
	return copilotHost
}

func setCopilotHost(v string) {
	copilotHostMu.Lock()
	copilotHost = v
	copilotHostMu.Unlock()
}

// workBuddyGlobalBase returns the international API host. Redirectable in tests
// alongside copilotHost so both domain branches can be exercised locally.
var (
	workBuddyGlobalMu sync.RWMutex
	workBuddyGlobal   = workBuddyGlobalDefault
)

func workBuddyGlobalBase() string {
	workBuddyGlobalMu.RLock()
	defer workBuddyGlobalMu.RUnlock()
	return workBuddyGlobal
}

func setWorkBuddyGlobalBase(v string) {
	workBuddyGlobalMu.Lock()
	workBuddyGlobal = v
	workBuddyGlobalMu.Unlock()
}

// workBuddyCredentials is the port of a2/b's C0368a credential record.
type workBuddyCredentials struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// workBuddyBaseURL ports a2/b.q(): the auth API host for the given domain.
//
// Note: the cn branch returns copilotHostValue() rather than the constant so the
// token-refresh endpoint follows the same test redirect as the login endpoints.
func workBuddyBaseURL(domain string) string {
	if isWorkBuddyGlobalDomain(domain) {
		return workBuddyGlobalBase()
	}
	return copilotHostValue()
}

// workBuddyOriginURL ports the a2/b.p() Origin/Referer selection:
//
//	D(c0368a).equals("global") ? "https://www.workbuddy.ai" : "https://www.codebuddy.cn"
func workBuddyOriginURL(domain string) string {
	if isWorkBuddyGlobalDomain(domain) {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// workBuddyHTTPClient performs the plugin's own HTTPS calls.
//
// CPA can route plugin traffic through a host transport policy, but the device
// code flow only needs plain POST/GET against Tencent, so a private client with
// a sane timeout is sufficient and keeps the plugin usable standalone.
var workBuddyHTTPClient = &http.Client{Timeout: 30 * time.Second}

// startWorkBuddyLogin ports V1/k.java:378 — ask Tencent for a device-code state
// plus the user-facing authorization URL.
func startWorkBuddyLogin() (authURL, state string, err error) {
	endpoint := copilotHostValue() + "/v2/plugin/auth/state?platform=CLI"
	req, errRequest := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if errRequest != nil {
		return "", "", errRequest
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codebuddyUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, errDo := workBuddyHTTPClient.Do(req)
	if errDo != nil {
		return "", "", fmt.Errorf("请求授权链接失败: %w", errDo)
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return "", "", fmt.Errorf("读取授权响应失败: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// V1/k.java throws "请求授权链接失败（HTTP <code>）".
		return "", "", fmt.Errorf("请求授权链接失败（HTTP %d）", resp.StatusCode)
	}

	stateValue, authURLValue, errParse := parseWorkBuddyStateResponse(body)
	if errParse != nil {
		return "", "", errParse
	}
	return authURLValue, stateValue, nil
}

// parseWorkBuddyStateResponse extracts state + authUrl from the state response.
//
// V1/k.java reads data["state"] and data["authUrl"] and rejects the response
// when either is empty ("授权响应缺少 state 或 authUrl").
func parseWorkBuddyStateResponse(body []byte) (state, authURL string, err error) {
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return "", "", errors.New("授权响应异常：" + truncateString(string(body), 200))
	}
	state = strings.TrimSpace(doc.Data.State)
	authURL = strings.TrimSpace(doc.Data.AuthURL)
	if state == "" || authURL == "" {
		// Some deployments return state/authUrl at the top level.
		var flat struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if errUnmarshal := json.Unmarshal(body, &flat); errUnmarshal == nil {
			if state == "" {
				state = strings.TrimSpace(flat.State)
			}
			if authURL == "" {
				authURL = strings.TrimSpace(flat.AuthURL)
			}
		}
	}
	if state == "" || authURL == "" {
		return "", "", errors.New("授权响应缺少 state 或 authUrl")
	}
	return state, authURL, nil
}

// pollWorkBuddyLogin ports N1/B.java:55 — poll the token endpoint until the user
// finishes signing in.
//
// Returns (creds, nil) on success, (nil, nil) while still pending.
func pollWorkBuddyLogin(state string) (*workBuddyCredentials, error) {
	endpoint := copilotHostValue() + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	req, errRequest := http.NewRequest(http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codebuddyUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, errDo := workBuddyHTTPClient.Do(req)
	if errDo != nil {
		// N1/B.java: on transport error it produces Y1.e(message).
		return nil, fmt.Errorf("轮询失败: %w", errDo)
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("轮询失败: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// N1/B.java: "轮询失败（HTTP <code>）".
		return nil, fmt.Errorf("轮询失败（HTTP %d）", resp.StatusCode)
	}

	var doc struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return nil, errors.New("轮询响应不是合法 JSON")
	}

	switch doc.Code {
	case workBuddyLoginCodePending:
		// N1/B.java: l3 == 11217 -> Y1.f.f4001a (keep waiting).
		return nil, nil
	case 0:
		if len(doc.Data) == 0 || string(doc.Data) == "null" {
			return nil, errors.New("登录响应缺少 data")
		}
		creds, errParse := parseWorkBuddyCredentials(doc.Data)
		if errParse != nil {
			return nil, errParse
		}
		return creds, nil
	default:
		msg := strings.TrimSpace(doc.Msg)
		if msg == "" {
			msg = fmt.Sprintf("登录未完成（code=%d）", doc.Code)
		}
		return nil, errors.New(msg)
	}
}

// parseWorkBuddyCredentials ports a2/b.t(): read the credential object, applying
// every documented field alias, then backfill uid/enterpriseId from the access
// token's JWT claims when the response omits them.
func parseWorkBuddyCredentials(raw []byte) (*workBuddyCredentials, error) {
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("解析登录凭据失败: %w", errUnmarshal)
	}

	creds := &workBuddyCredentials{
		AccessToken:  pickString(doc, "accessToken", "access_token"),
		RefreshToken: pickString(doc, "refreshToken", "refresh_token"),
		ExpiresAt:    pickInt64(doc, "expiresAt", "expires_at"),
		Domain:       pickString(doc, "domain"),
		UID:          pickString(doc, "uid", "userId", "user_id"),
		EnterpriseID: pickString(doc, "enterpriseId", "enterprise_id", "entId", "tenantId", "tenant_id"),
		Nickname:     pickString(doc, "nickname", "nickName", "name"),
	}

	if creds.AccessToken == "" {
		return nil, errors.New("登录响应缺少 accessToken")
	}

	// a2/b.t(): when uid is absent it is recovered from the JWT payload.
	if creds.UID == "" {
		creds.UID = jwtClaim(creds.AccessToken, "user_id", "userId", "uid", "sub")
	}
	if creds.EnterpriseID == "" {
		creds.EnterpriseID = jwtClaim(creds.AccessToken, "tenant_id", "tenantId", "enterprise_id", "enterpriseId")
	}
	if creds.Domain == "" {
		creds.Domain = workBuddyDefaultRegion
	}
	return creds, nil
}

// storageJSON ports a2/b.E(): the persisted credential shape the executor side
// reads back through ParseAuth.
//
// The "type" field is added on top of the original app's shape: CPA derives a
// credential's provider from metadata["type"] when it loads an auth file
// (internal/pluginhost/auth_callbacks.go:324), and without it every saved
// account would be filed under provider "unknown" and never match a
// codebuddy-model request.
func (c *workBuddyCredentials) storageJSON() []byte {
	raw, _ := json.Marshal(map[string]any{
		"type":         workBuddyProviderKey,
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
		"domain":       c.Domain,
		"uid":          c.UID,
		"enterpriseId": c.EnterpriseID,
		"nickname":     c.Nickname,
	})
	return raw
}

// authID ports a2/b.E():
//
//	if (uid.isEmpty()) uid = "codebuddy-" + k(accessToken.hashCode());
func (c *workBuddyCredentials) authID() string {
	if c.UID != "" {
		return c.UID
	}
	return fmt.Sprintf("%s-%d", workBuddyProviderKey, hashString(c.AccessToken))
}

// AuthKey is the cache key for credential-scoped data such as the model
// catalogue. It must change whenever the effective identity changes, so a
// refreshed token for the same user still hits the same cache slot.
func (c *workBuddyCredentials) AuthKey() string {
	return c.Domain + "/" + c.authID()
}

func (c *workBuddyCredentials) label() string {
	if c.Nickname != "" {
		return c.Nickname
	}
	if c.UID != "" {
		return workBuddyDisplayName + " " + c.UID
	}
	return workBuddyDisplayName
}

// expiresAtTime converts the epoch-seconds expiresAt into a time.Time, matching
// a2/b.c()'s use of expiresAt (seconds).
func (c *workBuddyCredentials) expiresAtTime() time.Time {
	if c.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(c.ExpiresAt, 0).UTC()
}

// refreshWorkBuddyToken ports a2/b.c(): POST {base}/v2/plugin/auth/token/refresh
// and merge the returned accessToken/expiresIn/refreshToken/domain.
func refreshWorkBuddyToken(creds *workBuddyCredentials) (*workBuddyCredentials, error) {
	if creds == nil || creds.RefreshToken == "" {
		return nil, errors.New("缺少 refreshToken，无法刷新")
	}
	endpoint := workBuddyBaseURL(creds.Domain) + "/v2/plugin/auth/token/refresh"
	req, errRequest := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if errRequest != nil {
		return nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)

	resp, errDo := workBuddyHTTPClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("刷新失败: %w", errDo)
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("刷新失败: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("刷新失败（HTTP %d）", resp.StatusCode)
	}

	var doc struct {
		AccessToken  string `json:"accessToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		RefreshToken string `json:"refreshToken"`
		Domain       string `json:"domain"`
		Code         int    `json:"code"`
		Msg          string `json:"msg"`
		Data         *struct {
			AccessToken  string `json:"accessToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			RefreshToken string `json:"refreshToken"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return nil, errors.New("刷新响应不是合法 JSON")
	}

	// Tolerate both the flat shape and the {"data":{...}} envelope.
	tokenNew, expiresIn, refreshNew, domainNew := doc.AccessToken, doc.ExpiresIn, doc.RefreshToken, doc.Domain
	if tokenNew == "" && doc.Data != nil {
		tokenNew, expiresIn, refreshNew, domainNew = doc.Data.AccessToken, doc.Data.ExpiresIn, doc.Data.RefreshToken, doc.Data.Domain
	}
	if tokenNew == "" {
		if doc.Msg != "" {
			return nil, errors.New(doc.Msg)
		}
		return nil, errors.New("刷新响应缺少 accessToken")
	}

	updated := *creds
	updated.AccessToken = tokenNew
	if refreshNew != "" {
		updated.RefreshToken = refreshNew
	}
	if expiresIn > 0 {
		updated.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	if domainNew != "" {
		updated.Domain = domainNew
	}
	return &updated, nil
}

// applyWorkBuddyHeaders ports a2/b.p(): the upstream header set, including the
// optional identity headers.
func applyWorkBuddyHeaders(h http.Header, creds *workBuddyCredentials) {
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("User-Agent", codebuddyUA)
	origin := workBuddyOriginURL(creds.Domain)
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
	h.Set("X-Product", "SaaS")
	if creds.UID != "" {
		h.Set("X-User-Id", creds.UID)
	}
	if creds.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", creds.EnterpriseID)
		h.Set("X-Tenant-Id", creds.EnterpriseID)
	}
	if creds.Domain != "" {
		h.Set("X-Domain", creds.Domain)
	}
}

// ---- tiny helpers ---------------------------------------------------------

func pickString(doc map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := doc[key]; ok {
			if s, okString := v.(string); okString && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func pickInt64(doc map[string]any, keys ...string) int64 {
	for _, key := range keys {
		raw, ok := doc[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case json.Number:
			if n, errInt := v.Int64(); errInt == nil {
				return n
			}
		}
	}
	return 0
}

// jwtClaim decodes the payload segment of a JWT and returns the first present
// claim, mirroring a2/b.t()'s recovery of uid / tenant id from the token.
func jwtClaim(token string, keys ...string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64URLDecode(parts[1])
	if errDecode != nil {
		return ""
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return pickString(claims, keys...)
}

// hashString reproduces Java's String.hashCode for the fallback auth id.
func hashString(s string) int32 {
	var h int32
	for _, r := range s {
		h = 31*h + int32(r)
	}
	return h
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ---- pending login registry ----------------------------------------------

// pendingLogin tracks one in-flight device-code login, keyed by state.
type pendingLogin struct {
	State     string
	AuthURL   string
	StartedAt time.Time
	ExpiresAt time.Time
}

type pendingLoginStore struct {
	mu    sync.Mutex
	items map[string]*pendingLogin
	max   int
}

func newPendingLoginStore() *pendingLoginStore {
	return &pendingLoginStore{items: make(map[string]*pendingLogin), max: 512}
}

func (s *pendingLoginStore) put(login *pendingLogin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= s.max {
		// Drop the oldest entry; device-code flows are short-lived.
		var oldestKey string
		var oldest time.Time
		for k, v := range s.items {
			if oldestKey == "" || v.StartedAt.Before(oldest) {
				oldestKey, oldest = k, v.StartedAt
			}
		}
		delete(s.items, oldestKey)
	}
	s.items[login.State] = login
}

func (s *pendingLoginStore) get(state string) (*pendingLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[state]
	return v, ok
}

func (s *pendingLoginStore) drop(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, state)
}
