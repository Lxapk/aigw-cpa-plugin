package main

// This file holds the built-in WorkBuddy model catalogue.
//
// Ported from the source app: a2/b.java declares a static fallback list used
// when the provider's own catalogue is unavailable.
//
//	a2/b.java:52
//	static final List f4223g = [ q(8, 128000, "deepseek-v4-flash", "DeepSeek V4 Flash"),
//	                             q(8, 128000, "deepseek-v4-pro",   "DeepSeek V4 Pro"),
//	                             q(8, 128000, "glm-5.2",           "GLM-5.2"),
//	                             q(8, 128000, "kimi-k2.5",         "Kimi-K2.5"),
//	                             q(8, 128000, "minimax-m2.5",      "MiniMax-M2.5") ]
//
// The `8` is the entry kind and `128000` the context window.
//
// Why the plugin needs this
//
// CPA's credential page shows "该凭证暂无可用模型" whenever ModelProvider
// returns an empty list. The live catalogue requires a working upstream call, so
// any transient failure (expired token, network blip, provider hiccup) makes a
// perfectly usable account look unusable. Returning the known list keeps the
// account visibly bound to its models; the live query still takes precedence
// whenever it succeeds, so new models appear as soon as the provider publishes
// them.

// fallbackModelContextWindow mirrors the 128000 declared in a2/b.java:52.
const fallbackModelContextWindow = 128000

// fallbackModels is the built-in catalogue from the source app.
var fallbackModels = []workBuddyModel{
	{ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", MaxInputTokens: fallbackModelContextWindow},
	{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro", MaxInputTokens: fallbackModelContextWindow},
	{ID: "glm-5.2", DisplayName: "GLM-5.2", MaxInputTokens: fallbackModelContextWindow},
	{ID: "kimi-k2.5", DisplayName: "Kimi-K2.5", MaxInputTokens: fallbackModelContextWindow},
	{ID: "minimax-m2.5", DisplayName: "MiniMax-M2.5", MaxInputTokens: fallbackModelContextWindow},
}

// fallbackModelsCopy returns a copy so callers cannot mutate the package list.
func fallbackModelsCopy() []workBuddyModel {
	out := make([]workBuddyModel, len(fallbackModels))
	copy(out, fallbackModels)
	return out
}

// modelsOrFallback prefers the live catalogue and falls back to the built-in
// list, so a credential is never presented as having no models.
func modelsOrFallback(live []workBuddyModel) ([]workBuddyModel, bool) {
	if len(live) > 0 {
		return live, false
	}
	return fallbackModelsCopy(), true
}
