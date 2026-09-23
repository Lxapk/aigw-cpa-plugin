package main

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
)

// gatewaySettings is a faithful port of V1.s (GatewaySettings) from
// AI 聚合网关 0.1.18.
//
// Original Kotlin data class field order / defaults:
//
//	port                  = 8790
//	apiKey                = ""
//	allowNoKey            = true
//	exposeLan             = true
//	onlyUsableModels      = false
//	refreshSkewSeconds    = 86400
//	maxRotate             = 3
//	quotaCooldownMillis   = 43200000   (12h)
//	softCooldownMillis    = 60000      (60s)
//	errorThreshold        = 3
//	errorCooldownMillis   = 600000     (10m)
//	logRetentionDays      = 30
//	defaultProvider       = "trae"
//
// Field names below use snake_case so that they can be declared directly as
// plugin ConfigFields in config.yaml.
type gatewaySettings struct {
	// Port is the original gateway listen port. CPA owns its own listener, so
	// this value is reported for parity only.
	Port int `json:"port" yaml:"port"`
	// APIKey is the client-facing bearer token (V1/o.j()).
	APIKey string `json:"api_key" yaml:"api_key"`
	// AllowNoKey mirrors allowNoKey: when true the Authorization header is not
	// required at all.
	AllowNoKey bool `json:"allow_no_key" yaml:"allow_no_key"`
	// ExposeLAN mirrors exposeLan (bind 0.0.0.0 vs 127.0.0.1). Reported only.
	ExposeLAN bool `json:"expose_lan" yaml:"expose_lan"`
	// OnlyUsableModels mirrors onlyUsableModels: hide models whose provider
	// marks them unavailable.
	OnlyUsableModels bool `json:"only_usable_models" yaml:"only_usable_models"`
	// RefreshSkewSeconds mirrors refreshSkewSeconds: refresh credentials this
	// far ahead of expiry.
	RefreshSkewSeconds int64 `json:"refresh_skew_seconds" yaml:"refresh_skew_seconds"`
	// MaxRotate mirrors maxRotate: how many credentials to try per request.
	MaxRotate int `json:"max_rotate" yaml:"max_rotate"`
	// QuotaCooldownMillis mirrors quotaCooldownMillis: hard cooldown after a
	// quota/balance rejection.
	QuotaCooldownMillis int64 `json:"quota_cooldown_millis" yaml:"quota_cooldown_millis"`
	// SoftCooldownMillis mirrors softCooldownMillis: cooldown applied after a
	// single transient failure.
	SoftCooldownMillis int64 `json:"soft_cooldown_millis" yaml:"soft_cooldown_millis"`
	// ErrorThreshold mirrors errorThreshold: consecutive failures required
	// before a credential is parked.
	ErrorThreshold int `json:"error_threshold" yaml:"error_threshold"`
	// ErrorCooldownMillis mirrors errorCooldownMillis: park duration once
	// ErrorThreshold is reached.
	ErrorCooldownMillis int64 `json:"error_cooldown_millis" yaml:"error_cooldown_millis"`
	// LogRetentionDays mirrors logRetentionDays.
	LogRetentionDays int `json:"log_retention_days" yaml:"log_retention_days"`
	// DefaultProvider mirrors defaultProvider: used when the requested model
	// carries no explicit "provider/model" prefix.
	DefaultProvider string `json:"default_provider" yaml:"default_provider"`
	// DefaultModel is the model used when the client asks for "auto" or omits
	// the model entirely, mirroring a2/b.java k()'s configured default.
	DefaultModel string `json:"default_model" yaml:"default_model"`
	// EnforceDefaultProvider, when true, rejects requests that address a
	// provider other than DefaultProvider. The original app always honoured an
	// explicit "provider/model" prefix, so this defaults to false.
	EnforceDefaultProvider bool `json:"enforce_default_provider" yaml:"enforce_default_provider"`
	// Debug enables verbose host logging.
	Debug bool `json:"debug" yaml:"debug"`
	// Checkin holds the daily check-in configuration (nested under "checkin").
	Checkin checkinSettings `json:"checkin" yaml:"checkin"`
	// Quota holds the quota-refresh configuration (nested under "quota").
	Quota quotaSettings `json:"quota" yaml:"quota"`
	// Routing holds the account-selection strategy (nested under "routing").
	Routing routingSettings `json:"routing" yaml:"routing"`
}

// defaultGatewaySettings returns the exact defaults of V1.s's synthetic
// no-arg constructor, plus this plugin's own additions.
func defaultGatewaySettings() gatewaySettings {
	return gatewaySettings{
		Port:                8790,
		APIKey:              "",
		AllowNoKey:          true,
		ExposeLAN:           true,
		OnlyUsableModels:    false,
		RefreshSkewSeconds:  86400,
		MaxRotate:           3,
		QuotaCooldownMillis: 43_200_000,
		SoftCooldownMillis:  60_000,
		ErrorThreshold:      3,
		ErrorCooldownMillis: 600_000,
		LogRetentionDays:    30,
		DefaultProvider:     "trae",
		Checkin:             defaultCheckinSettings(),
		Quota:               defaultQuotaSettings(),
		Routing:             defaultRoutingSettings(),
	}
}

// applyDefaults fills zero values with V1.s defaults, then normalises.
// It mirrors the guards used by V1.z.b() (port range, non-blank host).
func (g *gatewaySettings) applyDefaults() {
	d := defaultGatewaySettings()
	if g.Port <= 0 || g.Port >= 65536 {
		g.Port = d.Port
	}
	if g.RefreshSkewSeconds <= 0 {
		g.RefreshSkewSeconds = d.RefreshSkewSeconds
	}
	if g.MaxRotate < 1 {
		g.MaxRotate = d.MaxRotate
	}
	if g.QuotaCooldownMillis <= 0 {
		g.QuotaCooldownMillis = d.QuotaCooldownMillis
	}
	if g.SoftCooldownMillis <= 0 {
		g.SoftCooldownMillis = d.SoftCooldownMillis
	}
	if g.ErrorThreshold < 1 {
		g.ErrorThreshold = d.ErrorThreshold
	}
	if g.ErrorCooldownMillis <= 0 {
		g.ErrorCooldownMillis = d.ErrorCooldownMillis
	}
	if g.LogRetentionDays < 1 {
		g.LogRetentionDays = d.LogRetentionDays
	}
	g.DefaultProvider = strings.ToLower(strings.TrimSpace(g.DefaultProvider))
	if g.DefaultProvider == "" {
		g.DefaultProvider = d.DefaultProvider
	}
	g.APIKey = strings.TrimSpace(g.APIKey)

	// The check-in block is nested, so YAML decoding replaces it wholesale with
	// the zero value when the section is absent. Restore the defaults in that
	// case so an empty config does not silently schedule 00:00.
	g.Checkin.applyDefaults()
	// Same reasoning for the quota block.
	g.Quota.applyDefaults()
	// And the routing block.
	g.Routing.applyDefaults()
}

// applyDefaults fills the check-in block with sensible values when it was not
// configured. Everything except Enabled/OnStart is range-checked rather than
// defaulted, so an explicit 0 is still honoured for Minute.
func (c *checkinSettings) applyDefaults() {
	d := defaultCheckinSettings()
	if c.Hour < 0 || c.Hour > 23 {
		c.Hour = d.Hour
	}
	if c.Minute < 0 || c.Minute > 59 {
		c.Minute = d.Minute
	}
	// A completely unset block (all zero) is indistinguishable from "midnight"
	// in YAML, so treat "disabled + 00:00" as "not configured" and restore the
	// documented 09:00 default.
	if !c.Enabled && c.Hour == 0 && c.Minute == 0 {
		c.Hour = d.Hour
		c.Minute = d.Minute
	}
}

// settingsStore holds the live settings. The host re-sends the plugin config on
// plugin.register and plugin.reconfigure, so this is swapped atomically.
type settingsStore struct {
	mu  sync.RWMutex
	val gatewaySettings

	// registrations counts plugin.register / plugin.reconfigure calls so the
	// management page can show that hot-reload is wired up.
	registrations atomic.Int64
}

func newSettingsStore() *settingsStore {
	s := &settingsStore{}
	s.val = defaultGatewaySettings()
	return s
}

func (s *settingsStore) get() gatewaySettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.val
}

func (s *settingsStore) set(v gatewaySettings) {
	v.applyDefaults()
	s.mu.Lock()
	s.val = v
	s.mu.Unlock()
}

// setCheckin replaces only the check-in block, leaving gateway settings intact.
func (s *settingsStore) setCheckin(cfg checkinSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val.Checkin = cfg
}

// setQuota replaces only the quota block, leaving gateway settings intact.
func (s *settingsStore) setQuota(cfg quotaSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val.Quota = cfg
}

// setRouting replaces only the routing block, leaving gateway settings intact.
func (s *settingsStore) setRouting(cfg routingSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val.Routing = cfg
}

// lifecycleRequest is the payload CPA sends for plugin.register /
// plugin.reconfigure / plugin.quiesce.
//
//	rpcLifecycleRequest{
//	    ConfigYAML    []byte `json:"config_yaml"`
//	    SchemaVersion uint32 `json:"schema_version"`
//	}
//
// The ConfigYAML holds the raw YAML node the user wrote under
// plugins.configs.<plugin-id>, i.e. exactly the ConfigFields declared in the
// registration metadata.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// decodeLifecycleConfig parses the plugin config YAML node into settings.
//
// The incoming YAML is the flattened config namespace, e.g.
//
//	enabled: true
//	priority: 1
//	port: 8790
//	api_key: "sk-..."
//
// "enabled" and "priority" are host-owned and ignored here.
func (s *settingsStore) decodeLifecycleConfig(raw []byte) error {
	cfg := gatewaySettings{}
	if len(raw) > 0 {
		if errUnmarshal := yamlUnmarshalFlattened(raw, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	s.set(cfg)
	s.registrations.Add(1)
	return nil
}

// marshalForLog renders the settings for the management endpoint, redacting the
// API key the same way V1.s.toString() does.
func (g gatewaySettings) marshalForLog() map[string]any {
	out := map[string]any{}
	raw, errMarshal := json.Marshal(g)
	if errMarshal != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if g.APIKey != "" {
		out["api_key"] = "[REDACTED]"
	}
	return out
}
