package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's ModelRouter capability.
//
// Without a router, CPA has no reason to hand a chat request to a plugin
// executor: built-in providers claim their own models, and an unknown model
// name would fall through to "no provider". The router closes that gap by
// claiming requests whose model belongs to the WorkBuddy catalogue and routing
// them to this plugin's own executor (ModelRouteTargetSelf).
//
// The decision is deliberately conservative:
//
//	* only chat-completions source format is claimed
//	* the requested model must look like a WorkBuddy model
//
// so other providers keep working untouched.

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
		meta, okMeta := parseRequestMeta(req.Body)
		if okMeta {
			requested = meta.Model
		}
	}
	requested = normalizeRouteModel(requested)

	// An explicitly prefixed "codebuddy/<model>" always belongs to us, mirroring
	// the provider-prefix convention the source gateway used (V1/o.k step 6).
	if provider, model, ok := splitProviderPrefix(requested); ok {
		if isWorkBuddyProvider(provider) {
			return okEnvelope(pluginapi.ModelRouteResponse{
				Handled:     true,
				TargetKind:  pluginapi.ModelRouteTargetSelf,
				Reason:      "explicit codebuddy prefix",
				TargetModel: model,
			})
		}
		// Another provider was requested explicitly: do not interfere.
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	// Otherwise claim the request only if the model is in the known catalogue.
	if !isKnownWorkBuddyModel(requested) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetSelf,
		Reason:      "model present in WorkBuddy catalogue",
		TargetModel: requested,
	})
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
// configured credential. The cached catalogue is populated by model.for_auth,
// which CPA calls when it prepares a provider's model list.
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
