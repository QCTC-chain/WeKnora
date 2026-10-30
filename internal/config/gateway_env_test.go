package config

import "testing"

// TestApplyGatewayEnvOverrides pins the env-vs-YAML contract for
// GATEWAY_ENABLED: the OpenAI-compatible model gateway is opt-in, and the
// environment override must accept the usual truthy spellings while any
// other non-empty value disables the surface.
func TestApplyGatewayEnvOverrides(t *testing.T) {
	cases := []struct {
		name  string
		env   string // value for GATEWAY_ENABLED (empty == unset)
		yaml  bool   // pre-set gateway.enabled
		want  bool
	}{
		{"unset keeps YAML false", "", false, false},
		{"unset keeps YAML true", "", true, true},
		{"true enables", "true", false, true},
		{"TRUE enables", "TRUE", false, true},
		{"1 enables", "1", false, true},
		{"yes enables", "yes", false, true},
		{"on enables", "on", false, true},
		{"false disables YAML true", "false", true, false},
		{"0 disables YAML true", "0", true, false},
		{"no disables YAML true", "no", true, false},
		{"garbage disables YAML true", "banana", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GATEWAY_ENABLED", tc.env)
			cfg := &Config{Gateway: &GatewayConfig{Enabled: tc.yaml}}
			applyGatewayEnvOverrides(cfg)
			if cfg.Gateway.Enabled != tc.want {
				t.Fatalf("gateway.enabled = %v, want %v", cfg.Gateway.Enabled, tc.want)
			}
		})
	}
}

// TestApplyGatewayEnvOverridesCreatesSection verifies the nil-Gateway guard
// used by partially-built configs.
func TestApplyGatewayEnvOverridesCreatesSection(t *testing.T) {
	t.Setenv("GATEWAY_ENABLED", "true")
	cfg := &Config{}
	applyGatewayEnvOverrides(cfg)
	if cfg.Gateway == nil || !cfg.Gateway.Enabled {
		t.Fatalf("gateway section = %+v, want enabled", cfg.Gateway)
	}
}
