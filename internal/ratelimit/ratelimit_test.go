package ratelimit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func TestParse(t *testing.T) {
	full := `{"rate_limits":{"five_hour":{"used_percentage":42.5,"resets_at":1000},"seven_day":{"used_percentage":17,"resets_at":2000},"spend_limit":{"used_percentage":120,"resets_at":3000,"used_usd":12,"limit_usd":10}}}`
	s, err := Parse([]byte(full))
	if err != nil || s.FiveHour == nil || s.FiveHour.UsedPercent != 42.5 || s.FiveHour.ResetsAt != 1000 ||
		s.SevenDay == nil || s.SevenDay.UsedPercent != 17 || s.SpendLimit == nil || s.SpendLimit.UsedPercent != 120 ||
		s.SpendLimit.UsedUSD == nil || *s.SpendLimit.LimitUSD != 10 {
		t.Fatalf("full: %+v %v", s, err)
	}
	s, _ = Parse([]byte(`{"rate_limits":{"five_hour":{"used_percentage":0,"resets_at":5}}}`))
	if s.FiveHour == nil || s.FiveHour.UsedPercent != 0 || s.SevenDay != nil || s.SpendLimit != nil {
		t.Fatalf("partial: %+v", s)
	}
	for name, in := range map[string]string{
		"missing":   `{"model":"x"}`,
		"null":      `{"rate_limits":null}`,
		"empty":     `{"rate_limits":{}}`,
		"wrongtype": `{"rate_limits":{"five_hour":{"used_percentage":"42","resets_at":1},"seven_day":"x","spend_limit":[]}}`,
		"nopct":     `{"rate_limits":{"five_hour":{"resets_at":1}}}`,
	} {
		s, err := Parse([]byte(in))
		if err != nil && name != "wrongtype" || !s.Empty() {
			t.Errorf("%s: %+v %v", name, s, err)
		}
	}
	s, _ = Parse([]byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":"soon"}}}`))
	if s.FiveHour == nil || s.FiveHour.ResetsAt != 0 {
		t.Fatalf("bad reset: %+v", s)
	}
	if _, err := Parse([]byte("garbage")); err == nil {
		t.Fatal("want error")
	}
}

func TestSaveLoad(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, err := Load(st); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	in := Snapshot{FiveHour: &Window{42, 100}, Agent: "a", At: 7}
	Save(st, in)
	in2 := Snapshot{SevenDay: &Window{3, 200}, Agent: "b", At: 8}
	Save(st, in2)
	got, ok, err := Load(st)
	if err != nil || !ok || got.FiveHour != nil || got.SevenDay == nil || *got.SevenDay != *in2.SevenDay || got.Agent != "b" || got.At != 8 {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
}

func TestDetectAuth(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got := DetectAuth(get, home); got != "" {
		t.Fatalf("none: %q", got)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"subscriptionType":"max"}}`), 0o600)
	if got := DetectAuth(get, home); got != "oauth" {
		t.Fatalf("oauth: %q", got)
	}
	cfg := t.TempDir()
	env["CLAUDE_CONFIG_DIR"] = cfg
	if got := DetectAuth(get, home); got != "" {
		t.Fatalf("config dir without creds: %q", got)
	}
	env["ANTHROPIC_API_KEY"] = "sk"
	if got := DetectAuth(get, home); got != "api_key" {
		t.Fatalf("api key: %q", got)
	}
}

func TestSame(t *testing.T) {
	a := Snapshot{FiveHour: &Window{1, 2}, Agent: "a", At: 1}
	b := Snapshot{FiveHour: &Window{1, 2}, Agent: "b", At: 9}
	if !Same(a, b) {
		t.Fatal("should ignore agent/at")
	}
	b.FiveHour.UsedPercent = 2
	if Same(a, b) {
		t.Fatal("should differ")
	}
}
