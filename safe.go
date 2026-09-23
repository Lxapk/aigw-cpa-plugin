package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// This file hardens the plugin's background goroutines.
//
// Why this matters
//
// CPA fuses a plugin that panics: internal/pluginhost records the panic in its
// `fused` set, and every subsequent capability call for that plugin — including
// model_router and model_provider — is skipped. The user-visible result is
// baffling: an account that worked moments ago starts failing with
// "unknown provider for model ..." or shows "该凭证暂无可用模型", with nothing in
// the UI explaining why.
//
// The scheduler loops run outside any RPC frame, so a panic there is not caught
// by CPA's per-call recover for the call that happens to be in flight. Each loop
// therefore guards itself: a transient fault is contained, and the loop keeps
// running so the plugin stays healthy.

// safeGo runs fn in a goroutine, converting a panic into a diagnostic line.
//
// loopName identifies the goroutine in the message.
func safeGo(loopName string, fn func()) {
	go func() {
		defer recoverLoop(loopName)
		fn()
	}()
}

// recoverLoop converts a panic inside a loop iteration into a stderr line.
//
// It is deferred from inside the loop body as well as around the whole loop, so
// one bad iteration does not end the loop.
func recoverLoop(loopName string) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr,
			"aigw-reverse-proxy: recovered panic in %s: %v\n%s\n",
			loopName, r, debug.Stack())
	}
}

// guardLoop wraps one iteration of a background loop.
//
// Usage:
//
//	for {
//	    select { ... }
//	    guardLoop("quota", func() { doWork() })
//	}
func guardLoop(loopName string, fn func()) {
	defer recoverLoop(loopName)
	fn()
}

// guardRPC wraps an RPC handler so a panic inside the plugin cannot fuse every
// capability at once.
//
// CPA recovers plugin panics per call, but a recovered panic still marks the
// plugin fused, which is far more disruptive than returning an error for the one
// call. Recovering here keeps the failure local.
func guardRPC(method string, fn func() ([]byte, error)) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = errorEnvelope("plugin_panic",
				fmt.Sprintf("%s panicked: %v", method, r), 500)
			err = nil
			fmt.Fprintf(os.Stderr,
				"aigw-reverse-proxy: recovered panic in %s: %v\n%s\n",
				method, r, debug.Stack())
		}
	}()
	return fn()
}
