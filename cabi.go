// Package main implements the CLIProxyAPI (CPA) dynamic plugin
// "aigw-reverse-proxy".
//
// It re-implements the reverse-proxy behaviour extracted from the Android app
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
// pieces that made the AIGW gateway distinctive:
//
//   - client API-key gate            (FrontendAuthProvider)   <- V1/o.j()
//   - model -> provider routing      (RequestInterceptor)     <- V1/o.k() step 6-8
//   - model name rewriting           (RequestInterceptor)     <- V1/o.k() step 8
//   - upstream failure classification(ResponseInterceptor)    <- V1/o.k() step 9
//   - usage accounting / call log    (UsagePlugin + Management)<- V1/o.r()
//   - health/status surface          (ManagementAPI)          <- V1/s + V1/A0.s
package main

/*
#include <stdint.h>
#include <stdlib.h>

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
*/
import "C"

import (
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
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
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
