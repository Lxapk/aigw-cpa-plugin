package main

import (
	"encoding/json"
)

// host_rpc.go holds host-RPC plumbing that must compile without cgo.
//
// cabi.go carries `import "C"`, so it is excluded entirely when the build runs
// with CGO_ENABLED=0 — which is exactly how the unit tests and the CI "run unit
// tests" step are executed. Anything the rest of the plugin calls unconditionally
// therefore has to live outside the cgo file, with the C-side work injected via
// a function variable that cabi.go replaces during cliproxy_plugin_init.

// hostCallFunc performs one host RPC. cabi.go installs the real cgo-backed
// implementation at init time; the default reports the host as unavailable so
// the plugin stays usable (and testable) standalone.
var hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
	return nil, errHostUnavailable
}

// callHost invokes a host RPC (host.log, host.auth.save, ...).
func callHost(method string, payload any) (json.RawMessage, error) {
	return hostCallFunc(method, payload)
}

type errHostUnavailableType struct{}

func (errHostUnavailableType) Error() string { return "host callback unavailable" }

var errHostUnavailable = errHostUnavailableType{}

type errHostRPC struct {
	Code    string
	Message string
}

func (e errHostRPC) Error() string { return "host rpc " + e.Code + ": " + e.Message }
