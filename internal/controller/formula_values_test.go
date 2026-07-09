package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFormulaValue(t *testing.T) {
	t.Run("default is raw string", func(t *testing.T) {
		v, err := parseFormulaValue("hello", "")
		require.NoError(t, err)
		require.Equal(t, "hello", v)
	})

	t.Run("explicit string is raw", func(t *testing.T) {
		v, err := parseFormulaValue("---not-yaml", "string")
		require.NoError(t, err)
		require.Equal(t, "---not-yaml", v)
	})

	t.Run("yaml deserializes to nested structure", func(t *testing.T) {
		v, err := parseFormulaValue("store:\n  store_id: gq\nvlan:\n  - id: 8\n    name: storehub\n", "yaml")
		require.NoError(t, err)
		m, ok := v.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "gq", m["store"].(map[string]any)["store_id"])
		vlan := m["vlan"].([]any)
		require.Len(t, vlan, 1)
		// YAML numbers decode as JSON does (float64), matching rawExtensionToMap.
		require.Equal(t, float64(8), vlan[0].(map[string]any)["id"])
	})

	t.Run("json deserializes", func(t *testing.T) {
		v, err := parseFormulaValue(`{"a":{"b":1}}`, "json")
		require.NoError(t, err)
		require.Equal(t, float64(1), v.(map[string]any)["a"].(map[string]any)["b"])
	})

	t.Run("invalid yaml errors", func(t *testing.T) {
		_, err := parseFormulaValue("a: [unclosed", "yaml")
		require.Error(t, err)
	})

	t.Run("unknown format errors", func(t *testing.T) {
		_, err := parseFormulaValue("x", "toml")
		require.Error(t, err)
	})
}
