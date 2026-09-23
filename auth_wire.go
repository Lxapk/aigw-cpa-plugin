package main

import (
	"encoding/json"
)

// This file defines the host.auth.* RPC wire types.
//
// Source of truth: CLIProxyAPI's own declarations, which the plugin must match
// exactly rather than guess:
//
//	sdk/pluginapi.HostAuthFileEntry
//	internal/pluginhost.rpcHostAuthListResponse{ Files []HostAuthFileEntry `json:"files"` }
//	internal/pluginhost.rpcHostAuthGetResponse{ ..., JSON json.RawMessage `json:"json"` }
//
// Getting a field name wrong here is silent: json.Unmarshal leaves the field
// zero and the plugin behaves as if there were no accounts. That is exactly
// what happened before this file existed — the list response nests under
// "files" (not "auths") and the single-credential response carries the payload
// as "json" (not "storage_json").

// hostAuthEntry mirrors pluginapi.HostAuthFileEntry.
type hostAuthEntry struct {
	// ID identifies the credential record.
	ID string `json:"id,omitempty"`
	// AuthIndex is the stable runtime credential index.
	AuthIndex string `json:"auth_index,omitempty"`
	// Name is the credential file name or runtime identifier.
	Name string `json:"name"`
	// Type is the credential provider type.
	Type string `json:"type,omitempty"`
	// Provider is the credential provider key.
	Provider string `json:"provider,omitempty"`
	// Label is the human-readable credential label.
	Label string `json:"label,omitempty"`
	// Status is the current credential status.
	Status string `json:"status,omitempty"`
	// StatusMessage carries the latest status detail.
	StatusMessage string `json:"status_message,omitempty"`
	// Disabled reports whether the credential is disabled.
	Disabled bool `json:"disabled,omitempty"`
	// Unavailable reports whether the credential is currently unavailable.
	Unavailable bool `json:"unavailable,omitempty"`

	// StorageJSON is not part of HostAuthFileEntry: the list response omits the
	// credential bodies, which must be fetched per entry via host.auth.get.
	// It is populated here by decodeAuthList so callers keep working unchanged.
	StorageJSON json.RawMessage `json:"storage_json,omitempty"`
}

// hostAuthListResponse mirrors rpcHostAuthListResponse.
//
// The entries live under "files". Older or alternative hosts may use other
// spellings, so decodeAuthEntries also accepts those as a fallback.
type hostAuthListResponse struct {
	Files []hostAuthEntry `json:"files"`
	// Fallbacks for hosts that name it differently.
	Auths []hostAuthEntry `json:"auths,omitempty"`
	Items []hostAuthEntry `json:"items,omitempty"`
}

// hostAuthGetResponse mirrors rpcHostAuthGetResponse.
type hostAuthGetResponse struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	// JSON is the credential body.
	JSON json.RawMessage `json:"json"`
	// Fallbacks for hosts that nest the body under another key.
	StorageJSON json.RawMessage `json:"storage_json,omitempty"`
}

// credentialJSON returns whichever payload field the host populated.
func (r hostAuthGetResponse) credentialJSON() json.RawMessage {
	if len(r.JSON) > 0 {
		return r.JSON
	}
	return r.StorageJSON
}
