package main

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlUnmarshalFlattened decodes the plugin config YAML node into out.
//
// CPA flattens plugins.configs.<plugin-id> into a plain YAML mapping that
// contains the host-owned keys (enabled, priority) plus every ConfigField the
// plugin declared. A plain yaml.Unmarshal with matching struct tags is exactly
// what is needed; the wrapper exists so the settings code stays free of the
// yaml dependency and so the shape is documented in one place.
func yamlUnmarshalFlattened(raw []byte, out any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	// CPA may hand back the whole instance node including nested keys; the
	// struct tags select only the fields this plugin understands.
	return yaml.Unmarshal([]byte(trimmed), out)
}

// yamlHasKey reports whether the flattened config YAML contains the named
// top-level key. Used to tell an explicit host setting apart from an absent
// one, so panel-only fields (variant_override) can be restored after a
// reconfigure instead of being clobbered by the host YAML.
func yamlHasKey(raw []byte, key string) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return false
	}
	var flat map[string]any
	if err := yaml.Unmarshal([]byte(trimmed), &flat); err != nil {
		return false
	}
	_, ok := flat[key]
	return ok
}

// atomicWriteFile writes data to path via a temp file + rename so a crash
// never leaves a half-written state file behind.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pluginStateDir returns the per-user directory used to persist panel-only
// plugin state (variant override, manual account disables). It mirrors CPA's
// own choice of os.UserConfigDir() so the file survives restarts; falls back
// to the working directory when the home config dir is unavailable (PRoot
// sandboxes without a writable HOME).
func pluginStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, pluginName)
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "."
}
