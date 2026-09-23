package main

import (
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
