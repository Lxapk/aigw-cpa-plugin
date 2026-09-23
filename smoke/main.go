// Command smoke loads dist/aigw-reverse-proxy.so through the same C ABI surface
// CLIProxyAPI's pluginhost uses (dlopen + cliproxy_plugin_init) and replays a
// real RPC conversation against it.
//
// It is the end-to-end proof that the produced shared library is loadable and
// speaks the host protocol, independent of the Go unit tests.
package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*init_fn)(cliproxy_host_api*, cliproxy_plugin_api*);

// C-side trampolines. cgo cannot invoke a function pointer held in a struct
// field, so every indirect call goes through these.
static int call_init(void* fn, cliproxy_host_api* host, cliproxy_plugin_api* plugin) {
	return ((init_fn)fn)(host, plugin);
}
static int call_plugin(cliproxy_plugin_api* api, char* method, uint8_t* req, size_t reqLen, cliproxy_buffer* resp) {
	return api->call(method, req, reqLen, resp);
}
static void free_plugin_buffer(cliproxy_plugin_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}
static void call_shutdown(cliproxy_plugin_api* api) {
	api->shutdown();
}
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unsafe"
)

// hostCall documents the host callback signature the plugin may invoke. This
// smoke test wires a callback-less host (nil function pointers), which the
// plugin tolerates because none of the exercised code paths emit host RPCs.
func main() {
	libPath := "dist/aigw-reverse-proxy.so"
	if len(os.Args) > 1 {
		libPath = os.Args[1]
	}

	cPath := C.CString(libPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.dlopen(cPath, C.RTLD_NOW|C.RTLD_LOCAL)
	if handle == nil {
		die("dlopen failed: %s", C.GoString(C.dlerror()))
	}
	defer C.dlclose(handle)
	ok("dlopen %s", libPath)

	initSym := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(initSym))
	initPtr := C.dlsym(handle, initSym)
	if initPtr == nil {
		die("dlsym cliproxy_plugin_init failed: %s", C.GoString(C.dlerror()))
	}

	// The plugin performs no host callbacks on the code paths exercised here, so
	// a host API table with no function pointers is sufficient and matches how
	// pluginhost tolerates a callback-less host.
	var host C.cliproxy_host_api
	host.abi_version = 1
	host.host_ctx = nil
	host.call = nil
	host.free_buffer = nil

	var plugin C.cliproxy_plugin_api
	rc := C.call_init(initPtr, &host, &plugin)
	if rc != 0 {
		die("cliproxy_plugin_init returned %d", int(rc))
	}
	ok("cliproxy_plugin_init -> 0 (abi_version=%d)", uint32(plugin.abi_version))
	if plugin.call == nil || plugin.free_buffer == nil || plugin.shutdown == nil {
		die("plugin ABI table incomplete: call=%v free=%v shutdown=%v", plugin.call, plugin.free_buffer, plugin.shutdown)
	}

	// --- 1. register --------------------------------------------------
	regResp := call(plugin, "plugin.register", json.RawMessage(`{"schema_version":6,"config_yaml":"cG9ydDogOTEwMAphcGlfa2V5OiBzay1zbW9rZQphbGxvd19ub19rZXk6IGZhbHNlCmRlZmF1bHRfcHJvdmlkZXI6IHRyYWUK"}`))
	assertOK(regResp, "plugin.register")
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name         string `json:"Name"`
			Version      string `json:"Version"`
			ConfigFields []any  `json:"ConfigFields"`
		} `json:"metadata"`
		Capabilities map[string]any `json:"capabilities"`
	}
	mustUnmarshal(regResp.Result, &reg)
	if len(reg.Metadata.ConfigFields) != 16 {
		die("expected 16 config fields, got %d", len(reg.Metadata.ConfigFields))
	}
	ok("registered %s v%s (schema=%d, config_fields=%d)", reg.Metadata.Name, reg.Metadata.Version, reg.SchemaVersion, len(reg.Metadata.ConfigFields))
	for _, cap := range []string{"frontend_auth_provider", "request_interceptor", "response_interceptor", "response_stream_interceptor", "usage_plugin", "management_api"} {
		if v, present := reg.Capabilities[cap]; !present || v != true {
			die("capability %q not declared: %v", cap, reg.Capabilities)
		}
	}
	ok("all expected capabilities declared")

	// --- 2. frontend auth: no key must be rejected (allow_no_key=false) --
	authResp := call(plugin, "frontend_auth.authenticate", json.RawMessage(`{"Method":"POST","Path":"/v1/chat/completions"}`))
	if authResp.OK {
		die("frontend_auth.authenticate should have rejected a keyless request")
	}
	if authResp.Error == nil || authResp.Error.Code != "invalid_api_key" {
		die("unexpected auth error: %+v", authResp.Error)
	}
	ok("keyless request rejected: %s (%d)", authResp.Error.Code, authResp.Error.HTTPStatus)

	// --- 3. frontend auth: correct key must pass ------------------------
	authResp = call(plugin, "frontend_auth.authenticate", json.RawMessage(`{"Method":"POST","Path":"/v1/chat/completions","Headers":{"Authorization":["Bearer sk-smoke"]}}`))
	assertOK(authResp, "frontend_auth.authenticate(correct key)")
	var authOut struct {
		Authenticated bool `json:"authenticated"`
	}
	mustUnmarshal(authResp.Result, &authOut)
	if !authOut.Authenticated {
		die("correct key was not authenticated")
	}
	ok("correct key authenticated")

	// --- 4. request interception: provider routing + model rewrite ------
	reqResp := call(plugin, "request.intercept_before", json.RawMessage(`{"RequestID":"smoke-1","Body":"eyJtb2RlbCI6Im9wZW5haS9ncHQtNG8iLCJzdHJlYW0iOnRydWV9","Metadata":{"providers":["trae","openai"]}}`))
	assertOK(reqResp, "request.intercept_before")
	var intercepted struct {
		Terminate bool        `json:"Terminate"`
		Body      []byte      `json:"Body"`
		Headers   interface{} `json:"Headers"`
	}
	mustUnmarshal(reqResp.Result, &intercepted)
	if intercepted.Terminate {
		die("request should not have been terminated")
	}
	var rewritten map[string]any
	if errUnmarshal := json.Unmarshal(intercepted.Body, &rewritten); errUnmarshal != nil {
		die("rewritten body is not JSON: %v (%s)", errUnmarshal, intercepted.Body)
	}
	if rewritten["model"] != "gpt-4o" {
		die("model not rewritten to bare name: got %v (body=%s)", rewritten["model"], intercepted.Body)
	}
	if rewritten["stream"] != true {
		die("stream flag lost: %s", intercepted.Body)
	}
	ok("routed openai/gpt-4o -> provider=openai model=gpt-4o (prefix stripped, stream preserved)")

	// --- 5. response interception: usage accounting ---------------------
	respResp := call(plugin, "response.intercept_after", json.RawMessage(`{"RequestID":"smoke-1","StatusCode":200,"Model":"gpt-4o","RequestedModel":"openai/gpt-4o","RequestHeaders":{"X-Aigw-Provider":["openai"],"X-Aigw-Auth-Id":["acc-smoke"]},"Body":"eyJjaG9pY2VzIjpbeyJtZXNzYWdlIjp7ImNvbnRlbnQiOiJoaSJ9fV0sInVzYWdlIjp7InByb21wdF90b2tlbnMiOjEyLCJjb21wbGV0aW9uX3Rva2VucyI6NywidG90YWxfdG9rZW5zIjoxOX19"}`))
	assertOK(respResp, "response.intercept_after")
	ok("response intercepted and recorded")

	// --- 6. stream chunk interception ----------------------------------
	streamResp := call(plugin, "response.intercept_stream_chunk", json.RawMessage(`{"RequestID":"smoke-2","ChunkIndex":0,"Model":"gpt-4o","RequestHeaders":{"X-Aigw-Provider":["openai"]}}`))
	assertOK(streamResp, "response.intercept_stream_chunk(header init)")
	ok("stream header-init accepted")

	// --- 7. usage hook -------------------------------------------------
	usageResp := call(plugin, "usage.handle", json.RawMessage(`{"Provider":"openai","Model":"gpt-4o","AuthIndex":"acc-smoke","Stream":true,"RequestedAt":"2026-09-23T04:00:00Z","Latency":1500000000,"Detail":{"InputTokens":12,"OutputTokens":7,"TotalTokens":19}}`))
	assertOK(usageResp, "usage.handle")
	ok("usage recorded")

	// --- 8. management status ------------------------------------------
	mgmtResp := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/resource/plugins/aigw-reverse-proxy/status","Headers":{"Accept":["application/json"]}}`))
	assertOK(mgmtResp, "management.handle")
	var mgmt struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(mgmtResp.Result, &mgmt)
	if mgmt.StatusCode != 200 {
		die("management status code = %d", mgmt.StatusCode)
	}
	var statusDoc struct {
		Plugin struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			PortOwned bool   `json:"port_owned_by_cpa"`
		} `json:"plugin"`
		Settings struct {
			Port            int    `json:"port"`
			APIKey          string `json:"api_key"`
			DefaultProvider string `json:"default_provider"`
		} `json:"settings"`
		Usage struct {
			TotalCalls      int64 `json:"total_calls"`
			TotalPrompt     int64 `json:"total_prompt_tokens"`
			TotalCompletion int64 `json:"total_completion_tokens"`
		} `json:"usage"`
		Accounts []struct {
			Provider  string `json:"provider"`
			UID       string `json:"uid"`
			Successes int64  `json:"successes"`
		} `json:"accounts"`
	}
	mustUnmarshal(mgmt.Body, &statusDoc)
	if statusDoc.Settings.APIKey != "[REDACTED]" {
		die("api_key must be redacted in status output, got %q", statusDoc.Settings.APIKey)
	}
	if statusDoc.Settings.Port != 9100 {
		die("port from config_yaml not applied: got %d, want 9100", statusDoc.Settings.Port)
	}
	if statusDoc.Usage.TotalCalls != 2 {
		die("usage total_calls = %d, want 2", statusDoc.Usage.TotalCalls)
	}
	if statusDoc.Usage.TotalPrompt != 24 || statusDoc.Usage.TotalCompletion != 14 {
		die("usage tokens = %d/%d, want 24/14", statusDoc.Usage.TotalPrompt, statusDoc.Usage.TotalCompletion)
	}
	ok("status: port=%d default_provider=%s api_key=%s calls=%d tokens=%d/%d accounts=%d",
		statusDoc.Settings.Port, statusDoc.Settings.DefaultProvider, statusDoc.Settings.APIKey, statusDoc.Usage.TotalCalls,
		statusDoc.Usage.TotalPrompt, statusDoc.Usage.TotalCompletion, len(statusDoc.Accounts))

	// --- 9. WorkBuddy / codebuddy login (AuthProvider) ------------------
	// auth.identifier must return the provider key CPA derives the login
	// button from.
	identResp := call(plugin, "auth.identifier", nil)
	assertOK(identResp, "auth.identifier")
	var ident struct {
		Identifier string `json:"identifier"`
	}
	mustUnmarshal(identResp.Result, &ident)
	if ident.Identifier != "codebuddy" {
		die("auth identifier = %q, want codebuddy", ident.Identifier)
	}
	ok("auth.identifier -> %s", ident.Identifier)

	// auth.parse with a stored credential blob.
	parseResp := call(plugin, "auth.parse", json.RawMessage(`{"Provider":"codebuddy","RawJSON":"eyJhY2Nlc3NUb2tlbiI6ImF0LXNtb2tlIiwicmVmcmVzaFRva2VuIjoicnQtc21va2UiLCJleHBpcmVzQXQiOjE4OTM0NTYwMDAsInVpZCI6InUtc21va2UiLCJuaWNrbmFtZSI6IlNtb2tlIn0="}`))
	assertOK(parseResp, "auth.parse")
	var parsed struct {
		Handled bool `json:"Handled"`
		Auth    struct {
			Provider string `json:"Provider"`
			ID       string `json:"ID"`
			Label    string `json:"Label"`
		} `json:"Auth"`
	}
	mustUnmarshal(parseResp.Result, &parsed)
	if !parsed.Handled || parsed.Auth.ID != "u-smoke" {
		die("auth.parse result unexpected: %s", parseResp.Result)
	}
	ok("auth.parse -> provider=%s id=%s label=%s", parsed.Auth.Provider, parsed.Auth.ID, parsed.Auth.Label)

	// auth.login.start hits Tencent's real device-code endpoint. We only assert
	// that it either succeeds (returning a URL + state) or fails cleanly; the
	// sandbox may not reach copilot.tencent.com and the plugin must not crash.
	startResp := call(plugin, "auth.login.start", json.RawMessage(`{"Provider":"codebuddy"}`))
	if startResp.OK {
		var started struct {
			Provider string `json:"Provider"`
			URL      string `json:"URL"`
			State    string `json:"State"`
		}
		mustUnmarshal(startResp.Result, &started)
		if started.URL == "" || started.State == "" {
			die("auth.login.start returned empty url/state: %s", startResp.Result)
		}
		ok("auth.login.start -> state=%s url=%s", started.State, started.URL)

		// Polling a brand-new state should report "pending" (Tencent returns
		// code 11217) or a clean error if the network is unavailable.
		pollResp := call(plugin, "auth.login.poll", json.RawMessage(`{"Provider":"codebuddy","State":"`+started.State+`"}`))
		assertOK(pollResp, "auth.login.poll")
		var polled struct {
			Status  string `json:"Status"`
			Message string `json:"Message"`
		}
		mustUnmarshal(pollResp.Result, &polled)
		if polled.Status != "pending" && polled.Status != "error" {
			die("unexpected poll status %q", polled.Status)
		}
		ok("auth.login.poll -> status=%s", polled.Status)
	} else {
		// A clean error envelope is acceptable; a crash is not.
		ok("auth.login.start unreachable from sandbox (expected): %s", startErrCode(startResp))
	}

	// --- 11. model catalogue + execution capabilities --------------------
	// These four capabilities are what make WorkBuddy's models visible in
	// /v1/models and callable through /v1/chat/completions.
	var capsDoc struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustUnmarshal(regResp.Result, &capsDoc)
	for _, key := range []string{"model_provider", "model_router", "executor"} {
		if v, okCap := capsDoc.Capabilities[key]; !okCap || v != true {
			die("capability %q must be declared, got %v", key, capsDoc.Capabilities)
		}
	}
	ok("model_provider / model_router / executor declared (scope=%v)", capsDoc.Capabilities["executor_model_scope"])

	// executor.identifier
	execIdent := call(plugin, "executor.identifier", nil)
	assertOK(execIdent, "executor.identifier")
	var execIdentOut struct {
		Identifier string `json:"identifier"`
	}
	mustUnmarshal(execIdent.Result, &execIdentOut)
	if execIdentOut.Identifier != "codebuddy" {
		die("executor identifier = %q", execIdentOut.Identifier)
	}
	ok("executor.identifier -> %s", execIdentOut.Identifier)

	// model.for_auth with an unparseable credential must degrade to an empty
	// catalogue rather than an error, so the host keeps serving other providers.
	modelRes := call(plugin, "model.for_auth", json.RawMessage(`{"AuthProvider":"codebuddy","StorageJSON":"bm90IGpzb24="}`))
	assertOK(modelRes, "model.for_auth")
	var modelOut struct {
		Provider string `json:"Provider"`
		Models   []any  `json:"Models"`
	}
	mustUnmarshal(modelRes.Result, &modelOut)
	if modelOut.Provider != "codebuddy" {
		die("model.for_auth provider = %q", modelOut.Provider)
	}
	ok("model.for_auth -> provider=%s models=%d (graceful on bad auth)", modelOut.Provider, len(modelOut.Models))

	// model.route must defer to other providers and claim only our own models.
	routeRes := call(plugin, "model.route", json.RawMessage(`{"SourceFormat":"chat-completions","RequestedModel":"openai/gpt-4o"}`))
	assertOK(routeRes, "model.route(foreign)")
	var routeOut struct {
		Handled bool `json:"Handled"`
	}
	mustUnmarshal(routeRes.Result, &routeOut)
	if routeOut.Handled {
		die("model.route must not claim a foreign provider prefix")
	}
	ok("model.route defers foreign provider prefixes")

	routeRes = call(plugin, "model.route", json.RawMessage(`{"SourceFormat":"chat-completions","RequestedModel":"codebuddy/anything"}`))
	assertOK(routeRes, "model.route(own)")
	mustUnmarshal(routeRes.Result, &routeOut)
	if !routeOut.Handled {
		die("model.route must claim the explicit codebuddy/ prefix")
	}
	ok("model.route claims explicit codebuddy/ prefix")

	// --- 12. check-in capability ----------------------------------------
	// The check-in scheduler and endpoints live behind the management API.
	mgmtReg := call(plugin, "management.register", json.RawMessage(`{}`))
	assertOK(mgmtReg, "management.register")
	var mgmtRegOut struct {
		Resources []struct {
			Path string `json:"path"`
			Menu string `json:"menu"`
		} `json:"resources"`
	}
	mustUnmarshal(mgmtReg.Result, &mgmtRegOut)

	var sawCheckin bool
	for _, r := range mgmtRegOut.Resources {
		if r.Path == "/checkin" {
			sawCheckin = true
		}
	}
	if !sawCheckin {
		die("check-in resource not registered: %+v", mgmtRegOut.Resources)
	}
	ok("check-in resource registered (%d resources)", len(mgmtRegOut.Resources))

	// Status endpoint must answer with the expected fields.
	ckStatus := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/resource/plugins/aigw-reverse-proxy/checkin/status","Headers":{"Accept":["application/json"]}}`))
	assertOK(ckStatus, "management.handle(/checkin/status)")
	var ckStatusEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(ckStatus.Result, &ckStatusEnv)
	if ckStatusEnv.StatusCode != 200 {
		die("check-in status code = %d", ckStatusEnv.StatusCode)
	}
	var ckDoc map[string]any
	mustUnmarshal(ckStatusEnv.Body, &ckDoc)
	for _, key := range []string{"enabled", "hour", "minute", "running", "history"} {
		if _, okKey := ckDoc[key]; !okKey {
			die("check-in status missing %q: %s", key, ckStatusEnv.Body)
		}
	}
	ok("checkin/status -> enabled=%v hour=%v minute=%v", ckDoc["enabled"], ckDoc["hour"], ckDoc["minute"])

	// The HTML page must render the manual + automatic controls.
	ckPage := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/resource/plugins/aigw-reverse-proxy/checkin","Headers":{"Accept":["text/html"]}}`))
	assertOK(ckPage, "management.handle(/checkin)")
	var ckPageEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(ckPage.Result, &ckPageEnv)
	page := string(ckPageEnv.Body)
	for _, want := range []string{"手动签到", "自动签到", "立即为所有账号签到"} {
		if !strings.Contains(page, want) {
			die("check-in page missing %q", want)
		}
	}
	// The page must not use HTML forms: an HTML form can neither satisfy the
	// GET-only resource route nor attach the management key header, so it
	// either blank-pages or fails with "missing management key". The key is
	// kept in localStorage and sent via fetch() instead.
	if strings.Contains(page, "<form") {
		die("check-in page must not use HTML forms")
	}
	for _, want := range []string{"保存到浏览器", "aigw-management-key", "'Authorization': 'Bearer '"} {
		if !strings.Contains(page, want) {
			die("check-in page missing %q", want)
		}
	}
	ok("checkin page renders (localStorage key + controls) (%d bytes)", len(page))

	// The config endpoint the page's fetch() calls must work.
	ckCfg := call(plugin, "management.handle", json.RawMessage(`{"Method":"POST","Path":"/v0/management/aigw-reverse-proxy/checkin/config","Headers":{"Content-Type":["application/json"]},"Body":"eyJlbmFibGVkIjpmYWxzZSwiaG91ciI6OSwibWludXRlIjowfQ=="}`))
	assertOK(ckCfg, "management.handle(POST /checkin/config)")
	var ckCfgEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(ckCfg.Result, &ckCfgEnv)
	if ckCfgEnv.StatusCode != 200 {
		die("config POST status = %d", ckCfgEnv.StatusCode)
	}
	ok("checkin/config -> %d (%d bytes JSON)", ckCfgEnv.StatusCode, len(ckCfgEnv.Body))

	// Triggering a manual run without any host credentials must not crash: the
	// smoke host implements no host.auth.list, so the run reports the account
	// lookup failure cleanly.
	ckRun := call(plugin, "management.handle", json.RawMessage(`{"Method":"POST","Path":"/v0/resource/plugins/aigw-reverse-proxy/checkin/run"}`))
	assertOK(ckRun, "management.handle(/checkin/run)")
	var ckRunEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(ckRun.Result, &ckRunEnv)
	if ckRunEnv.StatusCode != 200 {
		die("check-in run status = %d (%s)", ckRunEnv.StatusCode, ckRunEnv.Body)
	}
	var runDoc struct {
		Trigger string `json:"trigger"`
		Total   int    `json:"total"`
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	mustUnmarshal(ckRunEnv.Body, &runDoc)
	if runDoc.Trigger != "manual" {
		die("run trigger = %q", runDoc.Trigger)
	}
	ok("checkin/run -> trigger=%s total=%d (graceful without host auth)", runDoc.Trigger, runDoc.Total)

	// --- 13. quota capability ------------------------------------------
	var capsDoc2 struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustUnmarshal(regResp.Result, &capsDoc2)
	if v, okCap := capsDoc2.Capabilities["quota_provider"]; !okCap || v != true {
		die("quota_provider capability must be declared, got %v", capsDoc2.Capabilities["quota_provider"])
	}
	ok("quota_provider capability declared")

	quotaIdent := call(plugin, "quota.identifier", nil)
	assertOK(quotaIdent, "quota.identifier")
	var quotaIdentOut struct {
		Identifier string `json:"identifier"`
	}
	mustUnmarshal(quotaIdent.Result, &quotaIdentOut)
	if quotaIdentOut.Identifier != "codebuddy" {
		die("quota identifier = %q", quotaIdentOut.Identifier)
	}
	ok("quota.identifier -> %s", quotaIdentOut.Identifier)

	quotaDesc := call(plugin, "quota.describe", json.RawMessage(`{}`))
	assertOK(quotaDesc, "quota.describe")
	var quotaDescOut struct {
		SupportedProviders []string `json:"supported_providers"`
		DisplayName        string   `json:"display_name"`
		SupportsReset      bool     `json:"supports_reset"`
	}
	mustUnmarshal(quotaDesc.Result, &quotaDescOut)
	if len(quotaDescOut.SupportedProviders) != 1 || quotaDescOut.SupportedProviders[0] != "codebuddy" {
		die("quota.describe providers = %v", quotaDescOut.SupportedProviders)
	}
	if quotaDescOut.SupportsReset {
		die("quota provider must not claim reset support")
	}
	ok("quota.describe -> providers=%v display=%s reset=%v",
		quotaDescOut.SupportedProviders, quotaDescOut.DisplayName, quotaDescOut.SupportsReset)

	// The quota page and its endpoints.
	quotaStatus := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/management/aigw-reverse-proxy/quota/status"}`))
	assertOK(quotaStatus, "management.handle(/quota/status)")
	var quotaStatusEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(quotaStatus.Result, &quotaStatusEnv)
	if quotaStatusEnv.StatusCode != 200 {
		die("quota status code = %d", quotaStatusEnv.StatusCode)
	}
	var quotaDoc map[string]any
	mustUnmarshal(quotaStatusEnv.Body, &quotaDoc)
	for _, key := range []string{"enabled", "interval_minutes", "total_credits", "accounts_known"} {
		if _, okKey := quotaDoc[key]; !okKey {
			die("quota status missing %q: %s", key, quotaStatusEnv.Body)
		}
	}
	ok("quota/status -> enabled=%v interval=%vmin total=%v",
		quotaDoc["enabled"], quotaDoc["interval_minutes"], quotaDoc["total_credits"])

	quotaPage := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/resource/plugins/aigw-reverse-proxy/quota","Headers":{"Accept":["text/html"]}}`))
	assertOK(quotaPage, "management.handle(/quota)")
	var quotaPageEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(quotaPage.Result, &quotaPageEnv)
	qp := string(quotaPageEnv.Body)
	for _, want := range []string{"已知额度合计", "立即刷新全部额度", "账号选用顺序"} {
		if !strings.Contains(qp, want) {
			die("quota page missing %q", want)
		}
	}
	if strings.Contains(qp, "<form") {
		die("quota page must not use HTML forms")
	}
	ok("quota page renders (%d bytes, localStorage key + no forms)", len(qp))

	// --- 15. combined single page + account listing ----------------------
	// The account list must be readable straight from the auth store, so a
	// freshly logged-in account shows up without any traffic, and only
	// WorkBuddy entries may appear.
	homeResp := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/resource/plugins/aigw-reverse-proxy/","Headers":{"Accept":["text/html"]}}`))
	assertOK(homeResp, "management.handle(/ combined page)")
	var homeEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(homeResp.Result, &homeEnv)
	if homeEnv.StatusCode != 200 || len(homeEnv.Body) == 0 {
		die("combined page status=%d len=%d", homeEnv.StatusCode, len(homeEnv.Body))
	}
	home := string(homeEnv.Body)
	for _, want := range []string{"管理密钥", "WorkBuddy 账号", "签到设置", "额度刷新设置", "调用统计", "网关设置"} {
		if !strings.Contains(home, want) {
			die("combined page missing %q", want)
		}
	}
	if strings.Contains(home, "<form") {
		die("combined page must not use HTML forms")
	}
	ok("combined page renders all sections (%d bytes, no forms)", len(home))

	accountsResp := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/management/aigw-reverse-proxy/accounts"}`))
	assertOK(accountsResp, "management.handle(/accounts)")
	var accountsEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(accountsResp.Result, &accountsEnv)
	if accountsEnv.StatusCode != 200 {
		die("accounts endpoint status = %d", accountsEnv.StatusCode)
	}
	var accountsDoc struct {
		Accounts []struct {
			AuthIndex string `json:"auth_index"`
		} `json:"accounts"`
		Total int `json:"total"`
	}
	mustUnmarshal(accountsEnv.Body, &accountsDoc)
	// This smoke host implements no host.auth.list, so the list is empty but
	// the endpoint must still answer cleanly.
	for _, a := range accountsDoc.Accounts {
		if !strings.HasPrefix(a.AuthIndex, "codebuddy") {
			die("account list leaked a non-WorkBuddy entry: %s", a.AuthIndex)
		}
	}
	ok("accounts endpoint -> total=%d (only WorkBuddy entries)", accountsDoc.Total)

	// --- 16. account-switching strategy (Scheduler) ----------------------
	var capsDoc3 struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustUnmarshal(regResp.Result, &capsDoc3)
	if v, okCap := capsDoc3.Capabilities["scheduler"]; !okCap || v != true {
		die("scheduler capability must be declared, got %v", capsDoc3.Capabilities["scheduler"])
	}
	ok("scheduler capability declared (account switching)")

	// A foreign provider must be left to the host scheduler.
	pickForeign := call(plugin, "scheduler.pick", json.RawMessage(`{"Provider":"anthropic","Candidates":[{"ID":"x","Provider":"anthropic","Status":"active"}]}`))
	assertOK(pickForeign, "scheduler.pick(foreign)")
	var pickForeignOut struct {
		Handled bool `json:"Handled"`
	}
	mustUnmarshal(pickForeign.Result, &pickForeignOut)
	if pickForeignOut.Handled {
		die("scheduler must not handle another provider")
	}
	ok("scheduler.pick defers foreign providers")

	// Our own provider with candidates must be handled.
	pickOwn := call(plugin, "scheduler.pick", json.RawMessage(`{"Provider":"codebuddy","Candidates":[{"ID":"a","Provider":"codebuddy","Status":"active"},{"ID":"b","Provider":"codebuddy","Status":"active"}]}`))
	assertOK(pickOwn, "scheduler.pick(own)")
	var pickOwnOut struct {
		Handled bool   `json:"Handled"`
		AuthID  string `json:"AuthID"`
	}
	mustUnmarshal(pickOwn.Result, &pickOwnOut)
	if !pickOwnOut.Handled || (pickOwnOut.AuthID != "a" && pickOwnOut.AuthID != "b") {
		die("scheduler.pick(own) = %+v", pickOwnOut)
	}
	ok("scheduler.pick -> auth=%s (handled)", pickOwnOut.AuthID)

	// The three strategies must be switchable from the management API.
	for _, strategy := range []string{"by_expiry", "round_robin", "random", "by_credits"} {
		body, _ := json.Marshal(map[string]string{"strategy": strategy})
		cfgResp := call(plugin, "management.handle", json.RawMessage(
			`{"Method":"POST","Path":"/v0/management/aigw-reverse-proxy/routing/config","Headers":{"Content-Type":["application/json"]},"Body":"`+
				base64Std(string(body))+`"}`))
		assertOK(cfgResp, "management.handle(/routing/config "+strategy+")")
	}
	ok("routing strategy switchable: by_expiry / round_robin / random / by_credits")

	routingResp := call(plugin, "management.handle", json.RawMessage(`{"Method":"GET","Path":"/v0/management/aigw-reverse-proxy/routing/status"}`))
	assertOK(routingResp, "management.handle(/routing/status)")
	var routingEnv struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	mustUnmarshal(routingResp.Result, &routingEnv)
	var routingDoc struct {
		Routing struct {
			Strategy string           `json:"strategy"`
			Options  []map[string]any `json:"options"`
		} `json:"routing"`
	}
	mustUnmarshal(routingEnv.Body, &routingDoc)
	if len(routingDoc.Routing.Options) != 4 {
		die("expected 4 strategy options, got %d", len(routingDoc.Routing.Options))
	}
	ok("routing/status -> strategy=%s options=%d", routingDoc.Routing.Strategy, len(routingDoc.Routing.Options))

	// The by_expiry strategy must be selectable and answer a pick request.
	//
	// Note: the rotation gate chain deliberately refuses to "switch" to the
	// account that is already current. After the earlier by_credits pick the
	// current account is "a", so a two-candidate request may legitimately come
	// back unhandled. A single candidate that is not current proves the path
	// works without depending on that state.
	setStrategy := call(plugin, "management.handle", json.RawMessage(`{"Method":"POST","Path":"/v0/management/aigw-reverse-proxy/routing/config","Headers":{"Content-Type":["application/json"]},"Body":"eyJzdHJhdGVneSI6ImJ5X2V4cGlyeSJ9"}`))
	assertOK(setStrategy, "routing/config by_expiry")
	expiryPick := call(plugin, "scheduler.pick", json.RawMessage(`{"Provider":"codebuddy","Candidates":[{"ID":"fresh-account","Provider":"codebuddy","Status":"active"}]}`))
	assertOK(expiryPick, "scheduler.pick(by_expiry)")
	var expiryOut struct {
		Handled bool   `json:"Handled"`
		AuthID  string `json:"AuthID"`
	}
	mustUnmarshal(expiryPick.Result, &expiryOut)
	if !expiryOut.Handled || expiryOut.AuthID != "fresh-account" {
		die("by_expiry must select a target it has not already chosen (got %+v)", expiryOut)
	}
	ok("by_expiry strategy selects an account (auth=%s)", expiryOut.AuthID)

	// The gate chain must refuse to re-select the current account, which is the
	// anti-flap behaviour ported from the reference implementation.
	repeatPick := call(plugin, "scheduler.pick", json.RawMessage(`{"Provider":"codebuddy","Candidates":[{"ID":"fresh-account","Provider":"codebuddy","Status":"active"}]}`))
	assertOK(repeatPick, "scheduler.pick(by_expiry, repeat)")
	var repeatOut struct {
		Handled bool `json:"Handled"`
	}
	mustUnmarshal(repeatPick.Result, &repeatOut)
	if repeatOut.Handled {
		die("by_expiry must not re-select the account that is already current")
	}
	ok("by_expiry gate chain refuses to re-select the current account")

	// Restore the default strategy for the remaining checks.
	restoreStrategy := call(plugin, "management.handle", json.RawMessage(`{"Method":"POST","Path":"/v0/management/aigw-reverse-proxy/routing/config","Headers":{"Content-Type":["application/json"]},"Body":"eyJzdHJhdGVneSI6ImJ5X2NyZWRpdHMifQ=="}`))
	assertOK(restoreStrategy, "routing/config by_credits")

	// --- 17. shutdown ---------------------------------------------------
	C.call_shutdown(&plugin)
	ok("cliproxy_plugin_shutdown returned cleanly")

	fmt.Println()
	fmt.Println("SMOKE TEST PASSED — the .so loads via dlopen and speaks the CPA plugin ABI.")
}

// call invokes one RPC through the loaded plugin's ABI table.
func call(plugin C.cliproxy_plugin_api, method string, payload json.RawMessage) envelope {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var reqPtr *C.uint8_t
	if len(payload) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(reqPtr))
	}

	var buf C.cliproxy_buffer
	rc := C.call_plugin(&plugin, cMethod, reqPtr, C.size_t(len(payload)), &buf)
	if buf.ptr != nil {
		defer C.free_plugin_buffer(&plugin, buf.ptr, buf.len)
	}
	if rc != 0 {
		die("%s: plugin call returned %d", method, int(rc))
	}
	if buf.ptr == nil {
		die("%s: empty response buffer", method)
	}
	raw := C.GoBytes(buf.ptr, C.int(buf.len))

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		die("%s: bad envelope %s: %v", method, raw, errUnmarshal)
	}
	fmt.Fprintf(os.Stderr, "  [rpc ] %-36s ok=%v\n", method, env.OK)
	return env
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error,omitempty"`
}

func assertOK(env envelope, what string) {
	if !env.OK {
		if env.Error != nil {
			die("%s failed: %s: %s", what, env.Error.Code, env.Error.Message)
		}
		die("%s failed: not ok", what)
	}
}

// startErrCode renders the error code of a failed auth.login.start envelope.
func startErrCode(env envelope) string {
	if env.Error == nil {
		return "unknown"
	}
	return env.Error.Code
}

// base64Std encodes bytes for embedding in a JSON request body, matching how
// Go marshals []byte.
func base64Std(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func mustUnmarshal(raw json.RawMessage, out any) {
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		die("unmarshal %s: %v", raw, errUnmarshal)
	}
}

func ok(format string, args ...any) {
	fmt.Printf("  ✓ "+format+"\n", args...)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  ✗ "+format+"\n", args...)
	os.Exit(1)
}
