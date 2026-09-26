package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's ModelProvider capability for WorkBuddy, which is
// what makes the models show up in /v1/models and in the management panel.
//
// Two entry points:
//
//	model.static   -> StaticModels   : models available without any credential
//	model.for_auth -> ModelsForAuth  : models for one concrete credential
//
// WorkBuddy's catalogue is always credential-scoped (a2/b.java w() needs a
// bearer token), so StaticModels returns the cached snapshot when available and
// nothing otherwise, while ModelsForAuth performs the real upstream call.

// modelCache memoises the per-credential catalogue so repeated /v1/models calls
// do not hammer the upstream. WorkBuddy's list is small and changes rarely.
type modelCacheEntry struct {
	models    []workBuddyModel
	fetchedAt time.Time
	err       error
}

var workBuddyModelCache = newModelCache()

type modelCache struct {
	mu  sync.Mutex
	ttl time.Duration
	// keyed by authID
	entries map[string]modelCacheEntry
}

func newModelCache() *modelCache {
	return &modelCache{ttl: 10 * time.Minute, entries: make(map[string]modelCacheEntry)}
}

func (c *modelCache) get(key string) ([]workBuddyModel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.fetchedAt) > c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	return entry.models, true
}

func (c *modelCache) put(key string, models []workBuddyModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = modelCacheEntry{models: models, fetchedAt: time.Now()}
}

func (c *modelCache) snapshot() []workBuddyModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]struct{})
	var out []workBuddyModel
	for _, entry := range c.entries {
		for _, m := range entry.models {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// modelStatic answers model.static.
//
// WorkBuddy cannot enumerate models unauthenticated, so the catalogue is built
// from the first usable credential. Reading only the cache would leave the list
// empty until something else triggered a fetch, which is why this falls back to
// a live query.
func modelStatic(request []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	_ = req

	models := workBuddyModelCache.snapshot()
	if len(models) == 0 {
		models = fetchCatalogueFromAnyCredential()
	}

	return okEnvelope(pluginapi.ModelResponse{
		Provider: workBuddyProviderKey,
		Models:   modelsToInfo(models),
	})
}

// fetchCatalogueFromAnyCredential queries the model catalogue using the first
// WorkBuddy credential it can find, and caches the result.
//
// It returns nil when there is no credential or the upstream call fails; the
// caller then reports an empty list rather than an error, so the host keeps
// serving other providers.
func fetchCatalogueFromAnyCredential() []workBuddyModel {
	for _, account := range listWorkBuddyAccounts() {
		if account.Disabled || account.Expired {
			continue
		}
		creds := account.credentials
		if creds == nil || creds.AccessToken == "" {
			continue
		}
		models, errList := workBuddyUpstream.listModels(context.Background(), creds)
		if errList != nil || len(models) == 0 {
			continue
		}
		workBuddyModelCache.put(creds.AuthKey(), models)
		return models
	}
	return nil
}

// modelForAuth answers model.for_auth: fetch the catalogue for one credential.
func modelForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	// Only handle our own provider; anything else is not ours to answer.
	if req.AuthProvider != "" && !isWorkBuddyProvider(req.AuthProvider) {
		return okEnvelope(pluginapi.ModelResponse{})
	}

	creds, errParse := parseWorkBuddyCredentials(req.StorageJSON)
	if errParse != nil {
		// The credential body could not be parsed (e.g. the host passed only
		// metadata). Report the built-in list rather than an empty response:
		// CPA shows "该凭证暂无可用模型" for an empty list, which hides a
		// credential that may well work.
		return okEnvelope(pluginapi.ModelResponse{
			Provider: workBuddyProviderKey,
			Models:   modelsToInfo(fallbackModelsCopy()),
		})
	}

	cacheKey := creds.AuthKey()
	if cached, ok := workBuddyModelCache.get(cacheKey); ok {
		return okEnvelope(pluginapi.ModelResponse{
			Provider: workBuddyProviderKey,
			Models:   modelsToInfo(cached),
		})
	}

	models, errList := workBuddyUpstream.listModels(context.Background(), creds)
	if errList != nil {
		// A failed live query must not make the credential look empty.
		models = fallbackModelsCopy()
	}
	models, usedFallback := modelsOrFallback(models)
	if !usedFallback {
		workBuddyModelCache.put(cacheKey, models)
	}

	return okEnvelope(pluginapi.ModelResponse{
		Provider: workBuddyProviderKey,
		Models:   modelsToInfo(models),
	})
}

// modelsToInfo converts the provider catalogue into CPA's ModelInfo shape.
func modelsToInfo(models []workBuddyModel) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	// The ID is prefixed with the provider key. Without it a bare
	// "deepseek-v4-flash" collides with the same model served by another plugin
	// (trae advertises "DeepSeek-V4-Flash"), and the merged list gives the
	// client no way to say which upstream it means. The prefix is also what
	// model.route already expects: splitProviderPrefix() accepts
	// "codebuddy/<model>", so the advertised and accepted names now agree.
	//
	// Casing is preserved: the upstream distinguishes models by exact spelling,
	// and the catalogue is the authority on it.
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		native := strings.TrimSpace(m.ID)
		if native == "" {
			continue
		}
		qualified := qualifyModelID(native)
		info := pluginapi.ModelInfo{
			// ID is what clients send in "model".
			ID:   qualified,
			Name: qualified,
			// Version carries the bare upstream name: it must not be sent with
			// the prefix, and it is useful in diagnostics.
			Version:     native,
			Object:      "model",
			OwnedBy:     workBuddyProviderKey,
			Type:        "chat",
			DisplayName: firstNonEmpty(m.DisplayName, native),
			Description: "WorkBuddy 上游模型（原生名 " + native + "）",
		}
		if m.MaxInputTokens > 0 {
			info.InputTokenLimit = m.MaxInputTokens
			info.ContextLength = m.MaxInputTokens
		}
		info.SupportedGenerationMethods = []string{"chat.completions"}
		out = append(out, info)
	}
	return out
}

// qualifyModelID prepends the provider key unless the name already carries it,
// so an ID never gains the prefix twice.
func qualifyModelID(native string) string {
	if strings.HasPrefix(strings.ToLower(native), workBuddyProviderKey+"/") {
		return native
	}
	return workBuddyProviderKey + "/" + native
}

// nativeModelID strips a recognised provider prefix, yielding the name the
// upstream expects. Unprefixed names pass through unchanged.
func nativeModelID(model string) string {
	model = strings.TrimSpace(model)
	if provider, rest, ok := splitProviderPrefix(model); ok && isWorkBuddyProvider(provider) {
		return rest
	}
	return model
}

// isWorkBuddyProvider reports whether a provider/type field names this plugin.
//
// The comparison is case-insensitive on purpose. CPA takes the value returned by
// auth.identifier, lower-cases it (pluginhost/auth_provider.go normalizes both
// the value it passes to auth.parse and the one it writes into the auth file),
// and can therefore record "workbuddy" where this plugin was created under
// "codebuddy". A case-sensitive switch silently stopped recognising the plugin's
// own accounts once the label became "WorkBuddy".
func isWorkBuddyProvider(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case workBuddyProviderKey, workBuddyDisplayNameLower:
		return true
	}
	return false
}

// workBuddyDisplayNameLower is the persisted form of the display name: CPA
// lower-cases whatever auth.identifier returned before storing it. It coincides
// with pluginName, which is why the routing key must not be returned from
// auth.identifier.
const workBuddyDisplayNameLower = "workbuddy"
