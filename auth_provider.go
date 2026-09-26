package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file wires WorkBuddy authentication into CPA's AuthProvider capability.
//
// CPA drives the flow through five RPCs:
//
//	auth.identifier   -> stable provider key
//	auth.parse        -> turn uploaded credential material into an AuthData
//	auth.login.start  -> begin a login, return the user-facing URL + state
//	auth.login.poll   -> poll until success, return the AuthData
//	auth.refresh      -> refresh an existing credential
//
// The management panel derives its "<provider>-auth-url" buttons from
// auth.identifier, so once this plugin registers AuthProvider the WorkBuddy
// login entry appears automatically.

var workBuddyPendingLogins = newPendingLoginStore()

// authIdentifier answers auth.identifier.
//
// CPA uses the value both to match the provider key and to label the OAuth
// entry on the Auth page. Matching is case-insensitive host-side
// (pluginhost/auth_provider.go: normalizeProviderID lowercases both sides), so
// returning the display spelling fixes the label — "workbuddy" became
// "WorkBuddy" — without changing which accounts are recognised.
//
// The routing key stays pluginName; see MethodFrontendAuthIdentifier.
func authIdentifier() ([]byte, error) {
	return okEnvelope(identifierResponse{Identifier: workBuddyDisplayName})
}

// authParse answers auth.parse: accept credential material the user pasted or
// uploaded and turn it into a CPA auth record.
//
// AI 聚合网关 stored WorkBuddy credentials as a JSON blob (a2/b.E()), so both
// that exact shape and a raw access-token string are accepted. The raw-token
// path is a convenience the original app did not have: it lets a user paste a
// token copied from the CLI tool without hand-building JSON.
func authParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	// Provider filter: only handle our own provider, let others pass.
	if req.Provider != "" && !strings.EqualFold(req.Provider, workBuddyProviderKey) {
		return okEnvelope(map[string]any{"Handled": false})
	}

	creds, errExtract := extractWorkBuddyCredentials(req)
	if errExtract != nil {
		return okEnvelope(map[string]any{
			"Handled": true,
			"Error":   errExtract.Error(),
		})
	}

	return okEnvelope(map[string]any{
		"Handled": true,
		"Auth":    workBuddyAuthData(creds),
	})
}

// extractWorkBuddyCredentials pulls credentials out of whatever the host
// supplied on the parse request.
//
// AuthParseRequest only carries RawJSON (plus path/file-name hints), so the
// credential material always arrives as bytes: either our own persisted JSON
// shape or a bare access token.
func extractWorkBuddyCredentials(req pluginapi.AuthParseRequest) (*workBuddyCredentials, error) {
	raw := strings.TrimSpace(string(req.RawJSON))
	if raw == "" {
		return nil, errors.New("未提供任何凭据内容")
	}

	// 1. Our own persisted JSON shape (or the {"data":{...}} envelope).
	if strings.HasPrefix(raw, "{") {
		if creds, errParse := parseWorkBuddyCredentials([]byte(raw)); errParse == nil {
			return creds, nil
		}
	}

	// 2. Bare access token (convenience: paste a token without building JSON).
	if looksLikeJWTOrToken(raw) {
		creds := &workBuddyCredentials{
			AccessToken: raw,
			Domain:      workBuddyDefaultRegion,
		}
		creds.UID = jwtClaim(raw, "user_id", "userId", "uid", "sub")
		creds.EnterpriseID = jwtClaim(raw, "tenant_id", "tenantId", "enterprise_id", "enterpriseId")
		creds.ExpiresAt = jwtNumericClaim(raw, "exp")
		return creds, nil
	}

	return nil, errors.New("无法识别的凭据格式：既不是 JSON 也不是访问令牌")
}

// looksLikeJWTOrToken is a permissive sanity check so obviously-wrong input
// (empty strings, whitespace, prose) is rejected early with a clear message.
func looksLikeJWTOrToken(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '~', r == '+', r == '/', r == '=':
			continue
		default:
			return false
		}
	}
	return true
}

// base64URLDecode accepts both padded and unpadded base64url input, which is
// what JWT payload segments vary between in practice.
func base64URLDecode(segment string) ([]byte, error) {
	if data, errDecode := base64.RawURLEncoding.DecodeString(segment); errDecode == nil {
		return data, nil
	}
	padded := segment
	if pad := len(padded) % 4; pad != 0 {
		padded += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(padded)
}

// jwtNumericClaim reads a numeric JWT claim such as exp.
func jwtNumericClaim(token, key string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, errDecode := base64URLDecode(parts[1])
	if errDecode != nil {
		return 0
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return 0
	}
	return pickInt64(claims, key)
}

// workBuddyAuthData converts credentials into CPA's AuthData record.
func workBuddyAuthData(creds *workBuddyCredentials) pluginapi.AuthData {
	authID := creds.authID()
	storage := creds.storageJSON()

	auth := pluginapi.AuthData{
		Provider:    workBuddyProviderKey,
		ID:          authID,
		FileName:    workBuddyProviderKey + "-" + sanitizeID(authID) + ".json",
		Label:       creds.label(),
		StorageJSON: storage,
		Metadata: map[string]any{
			"type":         workBuddyProviderKey,
			"display_name": workBuddyDisplayName,
			"domain":       creds.Domain,
		},
		Attributes: map[string]string{
			"provider": workBuddyProviderKey,
			"uid":      creds.UID,
		},
	}
	if exp := creds.expiresAtTime(); !exp.IsZero() {
		auth.NextRefreshAfter = exp
	}
	return auth
}

// authLoginStart answers auth.login.start: request a device code from Tencent
// and hand the user-facing URL back to CPA.
func authLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	provider := strings.TrimSpace(req.Provider)
	if provider != "" && !strings.EqualFold(provider, workBuddyProviderKey) &&
		!strings.EqualFold(provider, workBuddyDisplayName) {
		return errorEnvelope("unsupported_provider",
			"本插件仅支持 "+workBuddyDisplayName+"（"+workBuddyProviderKey+"）登录", 400), nil
	}

	// The realm decides which host issues the credential. A credential minted by
	// one realm is rejected by the other, so the choice is made here rather than
	// at request time.
	//
	// CPA exposes a single OAuth entry per plugin, so the version selector is
	// carried in Metadata. When the operator has not chosen, the global
	// 供应商切换 setting decides; if that is left on 全部供应商, the domestic channel is
	// used and the response says how to reach the other one.
	variant, explicit := authVariantResolve(req)

	started, errStart := beginWorkBuddyLogin(variant)
	if errStart != nil {
		return errorEnvelope("login_start_failed", errStart.Error(), 502), nil
	}

	// Spell out which channel this link belongs to and how to get the other, so
	// the single entry point is still usable for a mixed pool.
	hint := "在浏览器打开上面的链接并使用 " + variant.label() + " 账号登录。"
	if explicit {
		hint += "本次按你选择的方向签发凭据，登录后该账号只会走 " + variant.label() + " 的接口。"
	} else {
		hint += "当前未指定供应商，按「供应商切换」设置选择；如需另一侧，请先在设置里切换，或再次点击授权并指定供应商。"
	}

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  workBuddyProviderKey,
		URL:       started.URL,
		State:     started.State,
		ExpiresAt: started.ExpiresAt,
		Metadata: map[string]any{
			"display_name":  workBuddyDisplayName,
			"variant":       string(variant),
			"variant_label": variant.label(),
			"explicit":      explicit,
			"other_variant": string(otherVariant(variant)),
			"other_label":   otherVariant(variant).label(),
			"auth_host":     authHostFor(variant),
			"hint":          hint,
		},
	})
}

// workBuddyLoginStart is one issued login link.
type workBuddyLoginStart struct {
	State     string
	URL       string
	ExpiresAt time.Time
	Variant   wbVariant
}

// beginWorkBuddyLogin requests a device code for a realm and remembers the
// pending login so the poll can find it.
//
// Shared by the CPA-managed auth flow and the panel's own buttons, so both
// produce links that the same poller completes.
func beginWorkBuddyLogin(variant wbVariant) (workBuddyLoginStart, error) {
	authURL, state, errStart := startWorkBuddyLogin(variant)
	if errStart != nil {
		return workBuddyLoginStart{}, errStart
	}
	now := time.Now()
	expiresAt := now.Add(10 * time.Minute)
	workBuddyPendingLogins.put(&pendingLogin{
		State:     state,
		AuthURL:   authURL,
		StartedAt: now,
		ExpiresAt: expiresAt,
		Variant:   variant,
	})
	return workBuddyLoginStart{
		State:     state,
		URL:       authURL,
		ExpiresAt: expiresAt,
		Variant:   variant,
	}, nil
}

// panelAuthStart answers GET /workbuddy/auth/start?variant=cn|ai.
//
// CPA exposes exactly one OAuth entry per plugin (AuthProvider.Identifier
// returns a single string), and that entry already follows the 供应商切换
// setting: authVariantResolve() maps 国内版/国际版 to the matching login host.
//
// This endpoint covers the remaining case — adding an account for the *other*
// supplier without changing the global setting. It returns that realm's login
// link, and the state is registered so the normal poll completes it.
func panelAuthStart(req pluginapi.ManagementRequest) (managementResponse, bool) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method != http.MethodGet && method != http.MethodPost {
		return managementResponse{StatusCode: http.StatusMethodNotAllowed}, true
	}

	wanted := strings.TrimSpace(req.Query.Get("variant"))
	variant, ok := parseVariant(wanted)
	if !ok {
		// Without an explicit realm the call would be indistinguishable from the
		// OAuth entry itself, which already honours the setting.
		return managementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"ok":    false,
				"error": "请指定 variant=cn 或 variant=ai",
			}),
		}, true
	}

	started, errStart := beginWorkBuddyLogin(variant)
	if errStart != nil {
		return managementResponse{
			StatusCode: http.StatusBadGateway,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"ok":    false,
				"error": errStart.Error(),
			}),
		}, true
	}

	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonResponseHeaders(),
		Body: mustJSON(map[string]any{
			"ok":            true,
			"variant":       string(variant),
			"variant_label": variant.label(),
			"auth_host":     authHostFor(variant),
			"url":           started.URL,
			"state":         started.State,
			"expires_at":    started.ExpiresAt,
			"hint": "在浏览器打开该链接，使用" + variant.label() + "账号登录。" +
				"登录完成后回到 CPA 的授权页，或在本面板点击「刷新账号」查看结果。",
		}),
	}, true
}

// authVariantHint reads the realm hint from a login-start request.
//
// AuthLoginStartRequest has no dedicated realm field, so the choice arrives
// through Metadata. Several spellings are accepted so the caller does not have
// to match one exact key.
func authVariantHint(req pluginapi.AuthLoginStartRequest) string {
	for _, key := range []string{"variant", "realm", "region", "domain"} {
		if raw, ok := req.Metadata[key]; ok {
			if text, isString := raw.(string); isString && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

// authLoginPoll answers auth.login.poll: poll Tencent until the user finishes.
func authLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	state := strings.TrimSpace(req.State)
	if state == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "缺少 state，无法轮询登录状态",
		})
	}

	// Poll the realm that issued the state; the token endpoint is realm-scoped.
	pendingVariant := variantCn
	if pending, ok := workBuddyPendingLogins.get(state); ok && pending != nil && pending.Variant != "" {
		pendingVariant = pending.Variant
	}

	creds, errPoll := pollWorkBuddyLogin(state, pendingVariant)
	if errPoll != nil {
		workBuddyPendingLogins.drop(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		})
	}
	if creds == nil {
		// Still waiting — mirror N1/B.java's 11217 branch.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户在浏览器中完成登录…",
		})
	}

	workBuddyPendingLogins.drop(state)
	auth := workBuddyAuthData(creds)

	// Persist through the host so the credential lands in CPA's auth store,
	// exactly like the built-in providers do.
	if errSave := saveAuthThroughHost(auth); errSave != nil {
		// Persist failure is not fatal for the login itself; report it so the
		// operator can see why the account did not appear.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功，但写入 CPA 账号存储失败：" + errSave.Error(),
			Auth:    auth,
		})
	}

	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "WorkBuddy 账号登录成功",
		Auth:    auth,
	})
}

// authRefresh answers auth.refresh.
func authRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	creds, errParse := parseWorkBuddyCredentials(req.StorageJSON)
	if errParse != nil {
		return okEnvelope(map[string]any{
			"Error": "无法解析已有凭据：" + errParse.Error(),
		})
	}

	updated, errRefresh := refreshWorkBuddyToken(creds)
	if errRefresh != nil {
		return okEnvelope(map[string]any{"Error": errRefresh.Error()})
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             workBuddyAuthData(updated),
		NextRefreshAfter: updated.expiresAtTime(),
	})
}

// saveAuthThroughHost pushes a completed auth record into CPA's auth store.
//
// The request shape is exactly pluginapi.HostAuthSaveRequest:
//
//	{"name": "<file>.json", "json": <credential JSON>}
//
// CPA requires the name to end in ".json" and the payload to be a JSON object;
// the credential's provider is read from its "type" field, which storageJSON
// supplies.
func saveAuthThroughHost(auth pluginapi.AuthData) error {
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		fileName = workBuddyProviderKey + "-" + sanitizeID(auth.ID) + ".json"
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}

	storage := auth.StorageJSON
	if len(storage) == 0 {
		storage = []byte(`{"type":"` + workBuddyProviderKey + `"}`)
	}

	result, errCall := callHost("host.auth.save", map[string]any{
		"name": fileName,
		"json": json.RawMessage(storage),
	})
	if errCall != nil {
		return errCall
	}

	// A newly written credential must appear in the account list immediately,
	// without waiting for a request to exercise it.
	refreshAccountsAfterLogin()

	// CPA reports the physical file it wrote; log it for troubleshooting.
	var saved pluginapi.HostAuthSaveResponse
	if errUnmarshal := json.Unmarshal(result, &saved); errUnmarshal == nil {
		_ = saved
	}
	return nil
}

// sanitizeID makes an auth id safe to use inside a file name.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "default"
	}
	return out
}
