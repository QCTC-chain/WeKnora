package config

import (
	"testing"
	"time"
)

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

// TestGatewayRequestTimeoutOverride covers GATEWAY_REQUEST_TIMEOUT: Go
// durations, bare seconds, and garbage falling back to the default.
func TestGatewayRequestTimeoutOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"90s", 90 * time.Second},
		{"10m", 10 * time.Minute},
		{"120", 120 * time.Second},
		{"", 600 * time.Second},
		{"garbage", 600 * time.Second},
		{"-5s", 600 * time.Second},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("GATEWAY_REQUEST_TIMEOUT", tc.env)
			cfg := &Config{Gateway: &GatewayConfig{}}
			applyGatewayEnvOverrides(cfg)
			if got := cfg.Gateway.EffectiveRequestTimeout(); got != tc.want {
				t.Fatalf("request timeout = %v, want %v", got, tc.want)
			}
		})
	}
	// Nil section falls back to the default.
	if got := (*GatewayConfig)(nil).EffectiveRequestTimeout(); got != 600*time.Second {
		t.Fatalf("nil-section timeout = %v, want 600s", got)
	}
}
