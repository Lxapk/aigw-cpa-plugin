package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's ModelRouter capability.
//
// Without a router, CPA has no reason to hand a chat request to a plugin
// executor: it looks the model up in its own provider table via
// util.GetProviderName(), and an unknown name produces
//
//	unknown provider for model deepseek-v4.1-flash
//
// The router closes that gap by claiming requests whose model belongs to
// WorkBuddy.
//
// Claim criteria, in order of reliability:
//
//  1. an explicit "codebuddy/<model>" (or "workbuddy/<model>") prefix — the
//     client is telling us directly
//  2. the plugin has a WorkBuddy credential (AvailableProviders / auth store),
//     which is what makes the model servable at all
//  3. the model appears in the discovered catalogue
//
// Criterion 2 matters because the catalogue is fetched lazily: relying on it
// alone means a request that arrives before any /v1/models call is not claimed
// and fails with "unknown provider". Any model name is accepted for a provider
// the plugin actually owns; the upstream rejects genuinely bogus names with a
// clear error, which is a better outcome than refusing to route at all.

// modelRoute answers model.route.
func modelRoute(request []byte) ([]byte, error) {
	var req pluginapi.ModelRouteRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	// Only claim OpenAI chat-completions traffic; WorkBuddy speaks that format
	// natively, and anything else would need translation we do not implement.
	if req.SourceFormat != "" && req.SourceFormat != "chat-completions" {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	requested := req.RequestedModel
	if requested == "" {
		if meta, okMeta := parseRequestMeta(req.Body); okMeta {
			requested = meta.Model
		}
	}
	requested = strings.TrimSpace(requested)

	// 1) explicit provider prefix --------------------------------------
	if provider, model, ok := splitProviderPrefix(requested); ok {
		if isWorkBuddyProvider(provider) {
			return routeSelf(model, "explicit codebuddy prefix")
		}
		// Another provider was requested explicitly: do not interfere.
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	resolved := normalizeRouteModel(requested)

	// 2) does this plugin own any usable credential? --------------------
	if !pluginHasWorkBuddyCredential(req) {
		// No WorkBuddy account is registered: the request cannot be served
		// here, so leave it to the host (it may belong to another provider).
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	// 3) is the model one we have seen? ---------------------------------
	// A hit is definitive; a miss is not, because the catalogue is fetched
	// lazily. Fall through to claiming, since the provider is ours.
	if isKnownWorkBuddyModel(resolved) {
		return routeSelf(resolved, "model present in WorkBuddy catalogue")
	}

	return routeSelf(resolved, "provider has a WorkBuddy credential")
}

// routeSelf builds a "handled by this plugin" decision.
func routeSelf(model, reason string) ([]byte, error) {
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetSelf,
		Reason:      reason,
		TargetModel: model,
	})
}

// pluginHasWorkBuddyCredential reports whether any WorkBuddy credential is
// registered.
//
// AvailableProviders (built-in provider keys with auth registered) is checked
// first because it is authoritative and cheap; the plugin's own account store
// is the fallback for hosts that do not populate it.
func pluginHasWorkBuddyCredential(req pluginapi.ModelRouteRequest) bool {
	for _, p := range req.AvailableProviders {
		if isWorkBuddyProvider(p) {
			return true
		}
	}
	// The account store reads the auth inventory directly and is cached for a
	// few seconds, so this stays cheap on the hot path.
	for _, a := range state.accounts.accounts() {
		if a.Disabled {
			continue
		}
		return true
	}
	return false
}

// normalizeRouteModel trims the requested model, treating "auto" as "let the
// configured default decide" (a2/b.java k()).
func normalizeRouteModel(requested string) string {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" || trimmed == "auto" {
		return strings.TrimSpace(state.settings.get().DefaultModel)
	}
	return trimmed
}

// splitProviderPrefix splits "provider/model" the same way the source gateway
// did (V1/o.k step 6): only when the slash is not at position zero.
func splitProviderPrefix(model string) (provider, rest string, ok bool) {
	idx := strings.Index(model, "/")
	if idx <= 0 {
		return "", model, false
	}
	return model[:idx], model[idx+1:], true
}

// isKnownWorkBuddyModel reports whether the model has been discovered for any
// configured credential. The cache is populated by model.for_auth, which CPA
// calls when it prepares a provider's model list.
func isKnownWorkBuddyModel(model string) bool {
	if model == "" {
		return false
	}
	for _, m := range workBuddyModelCache.snapshot() {
		if m.ID == model {
			return true
		}
	}
	return false
}
