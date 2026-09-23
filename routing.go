package main

import (
	"encoding/json"
	"strings"
)

// route is a faithful port of V1.A (Route) from AI 聚合网关 0.1.18.
//
//	Route(providerId, model)
type route struct {
	ProviderID string
	Model      string
}

// routingResult describes how a client request was resolved to an upstream
// (provider, model) pair, mirroring V1/o.k() steps 6-8.
type routingResult struct {
	// Provider is the resolved provider id (V1.A.providerId).
	Provider string
	// Model is the model name after the provider's alias/mapping rewrite
	// (V1.k.m -> provider.k()).
	Model string
	// RequestedModel is the raw model string the client sent.
	RequestedModel string
	// Explicit reports whether the client used the "provider/model" form.
	Explicit bool
	// Rewritten reports whether the model name was changed by the provider
	// mapping (V1/o.k() rewrites body.model in that case).
	Rewritten bool
}

// resolveRoute ports V1/o.k()'s model resolution.
//
// Original smali logic:
//
//	model := body["model"]                       // trimmed, empty => ""
//	if idx := indexOf(model, '/'); idx > 0 {
//	    providerID := model[:idx]
//	    if provider := providers.byId(providerID); provider != nil {
//	        return Route(providerID, model[idx+1:])       // explicit form
//	    }
//	}
//	// fall back to defaultProvider
//	if provider := providers.byId(settings.defaultProvider); provider != nil {
//	    return Route(settings.defaultProvider, model)
//	}
//	// last resort: first registered provider
//	if first := providers.first(); first != nil {
//	    return Route(first.id(), model)
//	}
//	return nil  // => 400 "缺少 model 参数"
//
// The `idx > 0` guard is significant: a leading '/' is NOT treated as an
// explicit provider prefix.
func resolveRoute(rawModel string, settings gatewaySettings, known []string) (routingResult, bool) {
	model := strings.TrimSpace(rawModel)
	if model == "" {
		return routingResult{}, false
	}

	knownSet := make(map[string]struct{}, len(known))
	for _, id := range known {
		knownSet[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}

	// --- explicit "provider/model" form --------------------------------
	if idx := strings.Index(model, "/"); idx > 0 {
		providerID := strings.TrimSpace(model[:idx])
		if providerID != "" && containsFold(knownSet, providerID) {
			return routingResult{
				Provider:       strings.ToLower(providerID),
				Model:          model[idx+1:],
				RequestedModel: rawModel,
				Explicit:       true,
			}, true
		}
	}

	// --- default provider ----------------------------------------------
	if settings.DefaultProvider != "" {
		if _, ok := knownSet[settings.DefaultProvider]; ok {
			return routingResult{
				Provider:       settings.DefaultProvider,
				Model:          model,
				RequestedModel: rawModel,
			}, true
		}
	}

	// --- first registered provider -------------------------------------
	if len(known) > 0 {
		first := strings.ToLower(strings.TrimSpace(known[0]))
		if first != "" {
			return routingResult{
				Provider:       first,
				Model:          model,
				RequestedModel: rawModel,
			}, true
		}
	}

	return routingResult{}, false
}

func containsFold(set map[string]struct{}, v string) bool {
	if _, ok := set[strings.ToLower(strings.TrimSpace(v))]; ok {
		return true
	}
	return false
}

// rewriteModelBody ports the body mutation in V1/o.k():
//
//	body["model"] = provider.k(body["model"])   // only when the mapping differs
//
// It returns the rewritten body. When newModel equals the existing value the
// original bytes are returned untouched so that unrelated formatting is
// preserved.
func rewriteModelBody(body []byte, newModel string) ([]byte, bool) {
	if len(body) == 0 || newModel == "" {
		return body, false
	}
	var doc map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return body, false
	}
	currentRaw, ok := doc["model"]
	if !ok {
		return body, false
	}
	var current string
	if errUnmarshal := json.Unmarshal(currentRaw, &current); errUnmarshal != nil {
		return body, false
	}
	if current == newModel {
		return body, false
	}
	encoded, errMarshal := json.Marshal(newModel)
	if errMarshal != nil {
		return body, false
	}
	doc["model"] = encoded

	// Re-encode with a stable key order by round-tripping through an ordered
	// JSON object is overkill here; json.Marshal on a map is deterministic
	// (Go sorts map keys) which keeps logs diff-friendly.
	out, errMarshalAll := json.Marshal(doc)
	if errMarshalAll != nil {
		return body, false
	}
	return out, true
}

// requestMeta is the subset of the OpenAI chat-completions body the ported
// gateway logic inspects (V1/o.k() reads exactly "stream" and "model").
type requestMeta struct {
	Stream bool
	Model  string
}

func parseRequestMeta(body []byte) (requestMeta, bool) {
	if len(body) == 0 {
		return requestMeta{}, false
	}
	var doc struct {
		Stream *bool  `json:"stream"`
		Model  string `json:"model"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return requestMeta{}, false
	}
	meta := requestMeta{Model: doc.Model}
	if doc.Stream != nil {
		meta.Stream = *doc.Stream
	}
	return meta, true
}
