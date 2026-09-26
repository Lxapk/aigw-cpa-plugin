package main

import (
	"strings"
	"sync"
)

// This file is the single source of truth for WorkBuddy's two service variants
// (国内版 cn / 国际版 ai), ported from the reference implementation
// changexbc/workbuddy-switch (crates/wb-switch-core/src/modules/variant.rs).
//
// Design rule taken from that project: every variant difference (endpoint,
// billing path, OAuth platform, product domain) is declared here and nowhere
// else. Other files must not hardcode a variant-specific literal.
//
// Variant selection matches both the reference implementation and the APK's
// a2/b.java:284 D(): a credential whose domain ends with ".workbuddy.ai" is the
// international build; anything else is the domestic build.

// wbVariant identifies which WorkBuddy service a credential belongs to.
type wbVariant string

const (
	// variantCn is 国内版 (China mainland). It is the default, matching the
	// reference implementation's `_ => Self::Cn` fallback.
	variantCn wbVariant = "cn"
	// variantAi is 国际版 (international).
	variantAi wbVariant = "ai"
)

// allVariants lists both variants.
var allVariants = []wbVariant{variantCn, variantAi}

// variantForDomain resolves the variant from a credential's stored domain.
//
// Mirrors has_ai_domain_suffix(): only a real ".workbuddy.ai" suffix counts, so
// lookalike domains ("workbuddy.ai.evil") are not misclassified.
//
// Deprecated: prefer variantForCredentials, which also consults the JWT issuer.
// Many domestic credentials carry an empty domain, and the domain-only check
// silently labelled every one of them "cn" even when the token said otherwise.
func variantForDomain(domain string) wbVariant {
	override := state.settings.get().VariantOverride
	if override == "ai" {
		return variantAi
	}
	if override == "cn" {
		return variantCn
	}
	return variantFromDomainOnly(domain)
}

// variantFromDomainOnly is the domain half of the detection, with no override
// and no token signal. It is what the app's a2/b.java:284 D() does.
func variantFromDomainOnly(domain string) wbVariant {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return variantAi
	}
	return variantCn
}

// variantForCredentials resolves the variant of a stored credential.
//
// Signal order, ported from the reference implementation's
// detect_realm_from_token() (wb_accounts.py:162):
//
//  1. the domain, when it is a recognised WorkBuddy host;
//  2. the JWT issuer, which is the only signal left for credentials whose
//     domain field was never populated;
//  3. the default, which — unlike the app — is 国内版, because a credential
//     that reached this plugin came through the Tencent login flow unless its
//     domain explicitly says otherwise.
//
// The reference implementation treats any cn marker anywhere in the domain or
// issuer as decisive, so a genuinely ambiguous credential is classified cn
// rather than being left unclassified.
func variantForCredentials(creds *workBuddyCredentials) wbVariant {
	override := state.settings.get().VariantOverride
	if override == "ai" {
		return variantAi
	}
	if override == "cn" {
		return variantCn
	}
	if creds == nil {
		return variantCn
	}
	if isWorkBuddyGlobalDomain(creds.Domain) {
		return variantAi
	}
	if domainSaysCn(creds.Domain) {
		return variantCn
	}
	// No usable domain: fall back to the token's issuer, which the reference
	// implementation reads with the same substrings.
	switch issuerRealm(creds.AccessToken) {
	case "ai":
		return variantAi
	case "cn":
		return variantCn
	}
	return variantCn
}

// domainSaysCn reports whether the domain carries a positive domestic marker.
//
// A bare "codebuddy.cn"/"copilot.tencent.com" substring is enough here (matching
// the reference implementation) because the caller has already ruled out the
// international suffix.
func domainSaysCn(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	return strings.Contains(d, "codebuddy.cn") || strings.Contains(d, "copilot.tencent.com")
}

// issuerRealm classifies a JWT by its iss claim.
//
// Returns "" when the token is absent, unparseable, or its issuer is not a
// recognised WorkBuddy host — an unreadable issuer must not be mistaken for a
// positive international signal.
func issuerRealm(accessToken string) string {
	iss := strings.ToLower(strings.TrimSpace(jwtClaim(accessToken, "iss")))
	if iss == "" {
		return ""
	}
	if strings.Contains(iss, "copilot.tencent.com") || strings.Contains(iss, "codebuddy.cn") {
		return "cn"
	}
	if strings.Contains(iss, "workbuddy.ai") || strings.Contains(iss, "codebuddy.ai") {
		return "ai"
	}
	return ""
}

// detectVariantFromToken classifies a freshly issued token the way the
// reference implementation's detect_realm_from_token does, so a login can be
// labelled before any credential file exists.
func detectVariantFromToken(accessToken, domain string) (wbVariant, string) {
	if isWorkBuddyGlobalDomain(domain) {
		return variantAi, "domain"
	}
	if domainSaysCn(domain) {
		return variantCn, "domain"
	}
	if realm := issuerRealm(accessToken); realm != "" {
		if realm == "ai" {
			return variantAi, "issuer"
		}
		return variantCn, "issuer"
	}
	return variantCn, "default"
}

// isGlobalDomain keeps the historical helper name used elsewhere; it is the
// variant check expressed as a boolean.
func isGlobalDomain(domain string) bool {
	return variantForDomain(domain) == variantAi
}

// label renders the variant for the UI.
func (v wbVariant) label() string {
	if v == variantAi {
		return "国际版"
	}
	return "国内版"
}

// hasCheckin reports whether this variant exposes the daily check-in endpoint.
//
// Ported from the reference implementation's REALM_CONFIGS (wb_accounts.py:102
// and :114): the international build has no check-in at all ("has_checkin":
// False). Attempting one anyway spends a request that can only 404 and used to
// surface as a spurious "签到失败" for every international account.
func (v wbVariant) hasCheckin() bool {
	return v != variantAi
}

// apiBase is the WorkBuddy API host for this variant.
//
//	variant.rs: Self::Cn => WORKBUDDY_API_ENDPOINT ("https://www.codebuddy.cn")
//	            Self::Ai => AI_API_ENDPOINT ("https://www.workbuddy.ai")
func (v wbVariant) apiBase() string {
	if v == variantAi {
		return workBuddyGlobalBase()
	}
	return variantCnBase()
}

// chatBase is the host serving chat completions and the model catalogue.
//
// Unlike the billing endpoints, the domestic chat host is copilot.tencent.com
// (a2/b.java:717 q()), while the international one stays on workbuddy.ai.
func (v wbVariant) chatBase() string {
	if v == variantAi {
		return workBuddyGlobalBase()
	}
	return copilotHostValue()
}

// oauthPlatform is the `platform` query parameter used by the device-code
// login endpoints.
//
//	variant.rs: Self::Cn => WORKBUDDY_PLATFORM ("workbuddy")
//	            Self::Ai => AI_OAUTH_PLATFORM ("workbuddy-ai")
//
// The APK shipped "CLI", which is what the domestic endpoint accepted at the
// time; the reference implementation uses the desktop platform identifiers and
// sends no User-Agent override. Both are accepted by the upstream, but the
// variant-correct value is used here so the international build authenticates
// against the right product.
func (v wbVariant) oauthPlatform() string {
	if v == variantAi {
		return "workbuddy-ai"
	}
	return "workbuddy"
}

// productDomain is the CodeBuddy product domain used for Origin/Referer.
//
//	variant.rs: Cn => www.codebuddy.cn, Ai => www.codebuddy.ai
//
// Note the international product domain is codebuddy.**ai**, not
// workbuddy.ai — using the wrong one makes the origin look like a self-hosted
// deployment to CodeBuddy clients.
func (v wbVariant) productDomain() string {
	if v == variantAi {
		return "https://www.codebuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// billingPaths expands a billing path into the ordered candidates to try.
//
// Ported from variant.rs::billing_paths:
//
//	cn -> the path as-is (/v2/billing/meter/...)
//	ai -> first without the /v2 prefix (/billing/meter/...), then the original
//
// The international service was observed serving /billing/meter/... while the
// domestic one serves /v2/billing/meter/...; the order matters because only a
// 404 justifies trying the next candidate.
//
// A path that is not under the billing prefix has only one form and is returned
// unchanged — producing "/v2/v2/plugin/..." here would break every non-billing
// call.
func (v wbVariant) billingPaths(path string) []string {
	if v != variantAi {
		return []string{path}
	}
	rest, ok := strings.CutPrefix(path, billingPrefixCn)
	if !ok {
		// Not a billing path: no variant-specific rewriting applies.
		return []string{path}
	}
	primary := billingPrefixAi + rest
	return []string{primary, path}
}

const (
	// billingPrefixCn is the domestic billing prefix (config.CHECKIN_API_PREFIX).
	billingPrefixCn = "/v2/billing/meter"
	// billingPrefixAi is the international billing prefix.
	billingPrefixAi = "/billing/meter"
)

// productDomainFor maps a credential's raw domain onto the CodeBuddy product
// domain, ported from codebuddy_domain_for().
//
// WorkBuddy clients write "www.workbuddy.cn" / "www.workbuddy.ai", but CodeBuddy
// tooling only recognises its own product domains; anything else is treated as
// self-hosted and would read an "enterprise endpoint" setting. Mapping the two
// known WorkBuddy domains keeps the injected origin inside the recognised set.
// Other domains (enterprise/self-hosted) pass through untouched, and an empty
// domain falls back to the variant default.
func productDomainFor(domain string, v wbVariant) string {
	trimmed := strings.TrimSpace(domain)
	if trimmed == "" {
		return v.productDomain()
	}
	switch strings.ToLower(trimmed) {
	case "www.workbuddy.cn", "workbuddy.cn":
		return "https://www.codebuddy.cn"
	case "www.workbuddy.ai", "workbuddy.ai":
		return "https://www.codebuddy.ai"
	}
	return trimmed
}

// variantBaseOverride lets tests redirect the cn base.
var variantCnBaseValue = "https://www.codebuddy.cn"

func variantCnBase() string { return variantCnBaseValue }

func setVariantCnBase(v string) { variantCnBaseValue = v }

// variantTestMu guards concurrent redirects in tests.
var variantTestMu sync.RWMutex

// redirectAllCnBases points every cn-facing base at one URL.
//
// The plugin reaches codebuddy.cn through three different entry points
// (variantCnBase for billing, checkinBaseForTest for check-in, and the global
// override for the ai variant); tests need all of them moved together so a
// single httptest server can answer.
func redirectAllCnBases(url string) func() {
	variantTestMu.Lock()
	origVariant := variantCnBaseValue
	origCheckin := checkinBaseForTest()
	variantCnBaseValue = url
	workBuddyCheckinMu.Lock()
	workBuddyCheckinBaseCN = url
	workBuddyCheckinMu.Unlock()
	variantTestMu.Unlock()

	return func() {
		variantTestMu.Lock()
		variantCnBaseValue = origVariant
		workBuddyCheckinMu.Lock()
		workBuddyCheckinBaseCN = origCheckin
		workBuddyCheckinMu.Unlock()
		variantTestMu.Unlock()
	}
}
