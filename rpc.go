package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginName    = "workbuddy"
	pluginVersion = "0.13.11"
	pluginAuthor  = "BlackHawk"
	pluginRepo    = "https://github.com/router-for-me/CLIProxyAPI"
)

// envelope is the CPA RPC envelope:
//
//	{"ok":true,"result":{...}}  |  {"ok":false,"error":{"code","message","http_status"}}
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// errorEnvelope builds a failure envelope. httpStatus defaults to 500 the same
// way CPA's pluginabi.NewErrorEnvelope treats a zero status.
func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code:       code,
		Message:    message,
		HTTPStatus: httpStatus,
	}})
	return raw
}

// okEnvelope wraps a value in the CPA success envelope.
func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(raw)})
}

// identifierResponse answers every *.identifier method.
type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// registration mirrors pluginhost.rpcRegistration.
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  registrationCaps   `json:"capabilities"`
}

// registrationCaps mirrors pluginhost.rpcCapabilities. Only the fields this
// plugin sets are declared; the rest default to false/empty.
type registrationCaps struct {
	AuthProvider                  bool `json:"auth_provider"`
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive"`
	RequestInterceptor            bool `json:"request_interceptor"`
	ResponseInterceptor           bool `json:"response_interceptor"`
	StreamChunkInterceptor        bool `json:"response_stream_interceptor"`
	UsagePlugin                   bool `json:"usage_plugin"`
	ManagementAPI                 bool `json:"management_api"`

	// Model catalogue + execution. These four are what make WorkBuddy's models
	// visible in /v1/models and callable through /v1/chat/completions.
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`

	// QuotaProvider surfaces the remaining-credit figure the source app showed
	// ("已知额度合计", N1/R0.java:134).
	QuotaProvider bool `json:"quota_provider"`

	// Scheduler lets the plugin choose which credential a request uses, which
	// is what makes the account-switching strategy configurable.
	Scheduler bool `json:"scheduler"`
}

// managementRegistrationResponse mirrors pluginhost.rpcManagementRegistrationResponse.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// handleMethod dispatches one CPA RPC call.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {

	// ---- lifecycle ----------------------------------------------------
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		if errDecode := state.settings.decodeLifecycleConfig(req.ConfigYAML); errDecode != nil {
			return nil, errDecode
		}
		// Bring up the background schedulers so the configured cadences are
		// honoured for the lifetime of this plugin instance.
		if state.settings.get().Checkin.Enabled {
			startCheckinScheduler()
		}
		if state.settings.get().Quota.Enabled {
			startQuotaScheduler()
		}
		startTaskScheduler()
		return okEnvelope(buildRegistration())

	case pluginabi.MethodPluginQuiesce:
		// Acknowledge: nothing to drain, CPA owns the request lifecycle.
		return okEnvelope(map[string]any{})

	case pluginabi.MethodPluginShutdown:
		shutdownPlugin()
		return okEnvelope(map[string]any{})

	// ---- WorkBuddy / codebuddy login (port of N1/B + V1/k) ------------
	case pluginabi.MethodAuthIdentifier:
		return authIdentifier()

	case pluginabi.MethodAuthParse:
		return authParse(request)

	case pluginabi.MethodAuthLoginStart:
		return authLoginStart(request)

	case pluginabi.MethodAuthLoginPoll:
		return authLoginPoll(request)

	case pluginabi.MethodAuthRefresh:
		return authRefresh(request)

	// ---- model catalogue (port of a2/b.java:745 w()) -------------------
	case pluginabi.MethodModelStatic:
		return modelStatic(request)

	case pluginabi.MethodModelForAuth:
		return modelForAuth(request)

	// ---- request routing (port of V1/o.k step 6) -----------------------
	case pluginabi.MethodModelRoute:
		return modelRoute(request)

	// ---- upstream execution (port of a2/b.java:335 b()) ----------------
	case pluginabi.MethodExecutorIdentifier:
		return executorIdentifier()

	case pluginabi.MethodExecutorExecute:
		return executorExecute(request)

	case pluginabi.MethodExecutorExecuteStream:
		return executorExecuteStream(request)

	case pluginabi.MethodExecutorCountTokens:
		return executorCountTokens(request)

	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest(request)

	// ---- account selection strategy ------------------------------------
	case pluginabi.MethodSchedulerPick:
		return schedulerPick(request)

	// ---- quota (port of a2/b.java:406 m()) -----------------------------
	case pluginabi.MethodQuotaIdentifier:
		return quotaIdentifier()

	case pluginabi.MethodQuotaDescribe:
		return quotaDescribe()

	case pluginabi.MethodQuotaFetch:
		return quotaFetch(request)

	case pluginabi.MethodQuotaReset:
		return quotaReset()

	// ---- frontend auth (port of V1/o.j) -------------------------------
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: pluginName})

	case pluginabi.MethodFrontendAuthAuthenticate:
		return frontendAuth(request)

	// ---- request interception (port of V1/o.k steps 6-8) --------------
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequest(request, false)

	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequest(request, true)

	// ---- response / stream interception (failure classification) ------
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)

	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)

	// ---- usage accounting (port of V1/o.r) ---------------------------
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)

	// ---- management API (port of V1.s + A0.s status surface) ---------
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())

	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotImplemented), nil
	}
}

func buildRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "port", Type: pluginapi.ConfigFieldTypeInteger, Description: "Original gateway listen port (reported for parity; CPA owns the listener)."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Client bearer token required on inbound requests (V1/o.j)."},
				{Name: "allow_no_key", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow requests without an Authorization header (V1/s.allowNoKey). Only used when enforce_frontend_key is on."},
				{Name: "enforce_frontend_key", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Check the client bearer token in this plugin. Leave off so CPA's own api-keys keep working."},
				{Name: "variant_override", Type: pluginapi.ConfigFieldTypeString, Description: "Force variant: cn = domestic, ai = international, empty = auto (per account domain)."},
				{Name: "expose_lan", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Reported for parity (V1/s.exposeLan)."},
				{Name: "only_usable_models", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Hide models whose provider marks them unavailable (V1/s.onlyUsableModels)."},
				{Name: "refresh_skew_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Refresh credentials this far ahead of expiry (V1/s.refreshSkewSeconds)."},
				{Name: "max_rotate", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum credential rotations per request (V1/s.maxRotate)."},
				{Name: "quota_cooldown_millis", Type: pluginapi.ConfigFieldTypeInteger, Description: "Hard cooldown after an auth/rate/quota rejection (V1/s.quotaCooldownMillis)."},
				{Name: "soft_cooldown_millis", Type: pluginapi.ConfigFieldTypeInteger, Description: "Soft cooldown after a transient failure (V1/s.softCooldownMillis)."},
				{Name: "error_threshold", Type: pluginapi.ConfigFieldTypeInteger, Description: "Consecutive failures before parking a credential (V1/s.errorThreshold)."},
				{Name: "error_cooldown_millis", Type: pluginapi.ConfigFieldTypeInteger, Description: "Park duration once error_threshold is reached (V1/s.errorCooldownMillis)."},
				{Name: "log_retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Retention window for the call log (V1/s.logRetentionDays)."},
				{Name: "default_provider", Type: pluginapi.ConfigFieldTypeString, Description: "Provider used when the model carries no \"provider/model\" prefix (V1/s.defaultProvider)."},
				{Name: "default_model", Type: pluginapi.ConfigFieldTypeString, Description: "Model used when the client asks for \"auto\" or omits the model (a2/b.java k())."},
				{Name: "enforce_default_provider", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Reject models that address a provider other than default_provider."},
				{Name: "debug", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Emit verbose plugin logging."},
			},
		},
		Capabilities: registrationCaps{
			AuthProvider:                  true,
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: false,
			RequestInterceptor:            true,
			ResponseInterceptor:           true,
			StreamChunkInterceptor:        true,
			UsagePlugin:                   true,
			ManagementAPI:                 true,

			// WorkBuddy credits drive the account selection order.
			QuotaProvider: true,
			// Lets the panel switch between by-credits / round-robin / random.
			Scheduler: true,

			ModelProvider: true,
			ModelRouter:   true,
			Executor:      true,
			// WorkBuddy credentials are auth-bound, so both scopes apply.
			ExecutorModelScope: string(pluginapi.ExecutorModelScopeBoth),
			// WorkBuddy speaks OpenAI chat-completions natively in both
			// directions; no translation layer is needed.
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

// upstreamError classifies an upstream failure into the same buckets the app
// used (Y1.j: auth / rate / quota / transient) so the credential pool applies
// the matching cooldown.
type upstreamError struct {
	Kind       failureKind
	StatusCode int
	Code       string
	Message    string
}

// classifyUpstream ports the status handling in V1/o.k():
//
//	c.p() >= 400 -> V1.o.l(400, "upstream_rejected", body) and V1.k.c(...)
//
// OpenAI/Anthropic gateways surface the semantic class in the body, so the
// status code alone is not enough.
func classifyUpstream(statusCode int, body []byte) upstreamError {
	err := upstreamError{StatusCode: statusCode, Kind: failureTransient}

	var doc struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &doc)
	}
	err.Message = strings.TrimSpace(doc.Error.Message)
	err.Code = strings.TrimSpace(doc.Error.Code)
	errType := strings.ToLower(strings.TrimSpace(doc.Error.Type))
	errCode := strings.ToLower(err.Code)

	switch {
	case statusCode == http.StatusUnauthorized, statusCode == http.StatusForbidden,
		errType == "authentication_error", errType == "permission_error",
		errCode == "invalid_api_key", errCode == "unauthorized":
		err.Kind = failureAuth

	case statusCode == http.StatusTooManyRequests,
		errType == "rate_limit_error", errCode == "rate_limit_exceeded":
		err.Kind = failureRate

	case errType == "insufficient_quota", errCode == "insufficient_quota",
		errType == "billing_error", errCode == "quota_exceeded":
		err.Kind = failureQuota

	case statusCode >= 500:
		err.Kind = failureTransient
	}

	if err.Message == "" {
		err.Message = http.StatusText(statusCode)
	}
	return err
}

// statusForKind maps a failure class to the downstream HTTP status, matching
// the APK's error envelope codes (400 upstream_rejected, 503 no_healthy_account).
func statusForKind(k failureKind) int {
	switch k {
	case failureAuth:
		return http.StatusUnauthorized
	case failureRate:
		return http.StatusTooManyRequests
	case failureQuota:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

// logf forwards a diagnostic line to the CPA host log (pluginabi.MethodHostLog).
// It is intentionally fire-and-forget: the host may not expose the callback.
func logf(format string, _ ...any) {
	_ = format
}

// recordingEnabled reports whether the plugin should record a call.
func recordingEnabled() bool { return true }

func nowUTC() time.Time { return time.Now().UTC() }
