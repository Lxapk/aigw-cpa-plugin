package main

import (
	"context"
	"encoding/json"
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
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		info := pluginapi.ModelInfo{
			// ID is what clients send in "model"; we expose the provider-native
			// name so no mapping layer is needed.
			ID:          m.ID,
			Name:        m.ID,
			Object:      "model",
			OwnedBy:     workBuddyProviderKey,
			Type:        "chat",
			DisplayName: m.DisplayName,
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

// isWorkBuddyProvider accepts both the internal key and the display name.
func isWorkBuddyProvider(name string) bool {
	switch name {
	case workBuddyProviderKey, workBuddyDisplayName:
		return true
	}
	return false
}
