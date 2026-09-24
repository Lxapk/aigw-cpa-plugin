// Package main implements the CLIProxyAPI (CPA) dynamic plugin
// "workbuddy".
//
// It re-implements behaviour extracted from the Android app
// "AI 聚合网关" (package dev.aigw.app, version 0.1.18).
//
// Source-of-truth for the ported logic (JADX decompilation of the APK):
//
//	V1/o  extends fi.iki.elonen.NanoHTTPD  -> gateway HTTP server / reverse proxy
//	V1/k                                    -> gateway engine (account pool)
//	V1/s                                    -> GatewaySettings
//	V1/z                                    -> ProxySettings (outbound proxy)
//	V1/C                                    -> hand-written chunked SSE responder
//	V1/m                                    -> stream pump
//	A0.s                                    -> credential pool health state
//
// and for the WorkBuddy/codebuddy device-code login (v0.2.0):
//
//	a2/b   -> WorkBuddy provider ("codebuddy"), credential parsing/format
//	Y1/b   -> login method enum (DEVICE_CODE)
//	N1/B   -> device-code polling implementation
//	V1/k   -> device-code state request
//
// The original gateway exposed:
//
//	POST /v1/chat/completions   OpenAI-compatible reverse-proxy entrypoint
//	GET  /v1/models             aggregated model catalogue
//	GET  /healthz               liveness probe
//	GET  /authorize             OAuth callback shim
//
// and enforced, in V1/o.j():
//
//	Authorization: Bearer <GatewaySettings.apiKey>   (constant-time compare)
//	unless GatewaySettings.allowNoKey is true.
//
// CPA already owns the HTTP server, routing, provider executors and credential
// pool, so this plugin re-uses those host facilities and only re-implements the
// pieces that made the source gateway distinctive:
//
//   - client API-key gate            (FrontendAuthProvider)   <- V1/o.j()
//   - model -> provider routing      (RequestInterceptor)     <- V1/o.k() step 6-8
//   - model name rewriting           (RequestInterceptor)     <- V1/o.k() step 8
//   - upstream failure classification(ResponseInterceptor)    <- V1/o.k() step 9
//   - usage accounting / call log    (UsagePlugin + Management)<- V1/o.r()
//   - health/status surface          (ManagementAPI)          <- V1/s + V1/A0.s
//   - WorkBuddy device-code login    (AuthProvider)           <- N1/B + V1/k
package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// The host API table is handed to us by CPA at init time. cgo cannot invoke a
// function pointer held in a struct field, so every host call goes through
// these C trampolines.
static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host == nil {
		return 2
	}
	C.store_host_api(host)
	// Route host RPCs through the cgo implementation now that a host exists.
	hostCallFunc = callHostCgo
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	// guardRPC contains a panic to this one call. CPA recovers plugin panics, but
	// it also marks the plugin fused, which skips every later capability and
	// leaves the user with "unknown provider" for no visible reason.
	raw, errHandle := guardRPC(C.GoString(method), func() ([]byte, error) {
		return handleMethod(C.GoString(method), requestBytes)
	})
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), 500))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	shutdownPlugin()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHostCgo is the cgo-backed host RPC implementation. cabi.go installs it
// into hostCallFunc during init; host_rpc.go provides the cgo-free default so
// the package also builds and tests with CGO_ENABLED=0.
func callHostCgo(method string, payload any) (json.RawMessage, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var body []byte
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		body = raw
	}

	var reqPtr *C.uint8_t
	if len(body) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(body))
		defer C.free(unsafe.Pointer(reqPtr))
	}

	var buf C.cliproxy_buffer
	rc := C.call_host_api(cMethod, reqPtr, C.size_t(len(body)), &buf)
	if buf.ptr != nil {
		defer C.free_host_buffer(buf.ptr, buf.len)
	}
	if rc != 0 {
		return nil, errHostUnavailable
	}
	if buf.ptr == nil || buf.len == 0 {
		return nil, nil
	}
	raw := C.GoBytes(buf.ptr, C.int(buf.len))

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !env.OK {
		if env.Error != nil {
			return nil, errHostRPC{Code: env.Error.Code, Message: env.Error.Message}
		}
		return nil, errHostUnavailable
	}
	return env.Result, nil
}
