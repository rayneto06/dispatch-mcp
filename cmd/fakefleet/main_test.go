package main

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	base := map[string]string{"PARTNER_API_KEY": "key", "WEBHOOK_SECRET": "secret"}
	env := func(over ...string) func(string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(over); i += 2 {
			m[over[i]] = over[i+1]
		}
		return func(k string) string { return m[k] }
	}

	cfg, err := loadConfig(env())
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Addr != ":8090" || cfg.WebhookURL != "http://127.0.0.1:8081" || cfg.TimeScale != 1 {
		t.Errorf("defaults = %+v", cfg)
	}

	cfg, err = loadConfig(env("PORT", "9100", "WEBHOOK_URL", "https://example.test/", "TIME_SCALE", "0.1"))
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if cfg.Addr != ":9100" || cfg.WebhookURL != "https://example.test" || cfg.TimeScale != 0.1 {
		t.Errorf("custom = %+v", cfg)
	}

	for _, tt := range []struct{ name, key, value, want string }{
		{"missing api key", "PARTNER_API_KEY", "", "PARTNER_API_KEY"},
		{"missing secret", "WEBHOOK_SECRET", "", "WEBHOOK_SECRET"},
		{"bad time scale", "TIME_SCALE", "fast", "TIME_SCALE"},
		{"zero time scale", "TIME_SCALE", "0", "TIME_SCALE"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(env(tt.key, tt.value))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want mention of %s", err, tt.want)
			}
		})
	}
}
