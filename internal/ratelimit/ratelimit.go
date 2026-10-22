// Package ratelimit holds the latest subscription rate-limit snapshot that
// Claude Code reports to the statusLine command. All agents share one account,
// so the snapshot is a single KV entry and the last writer wins.
package ratelimit

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

// Key is the kv key holding the JSON snapshot.
const Key = "ratelimit"

// Window is one rate-limit window. ResetsAt is unix seconds (0 = unknown).
type Window struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

// SpendWindow is the gateway spend limit; USD fields are nil when not reported.
type SpendWindow struct {
	UsedPercent float64  `json:"used_percent"`
	ResetsAt    int64    `json:"resets_at"`
	UsedUSD     *float64 `json:"used_usd,omitempty"`
	LimitUSD    *float64 `json:"limit_usd,omitempty"`
}

// Snapshot is the last observed state. A nil window means unknown, never 0.
type Snapshot struct {
	FiveHour   *Window      `json:"five_hour,omitempty"`
	SevenDay   *Window      `json:"seven_day,omitempty"`
	SpendLimit *SpendWindow `json:"spend_limit,omitempty"`
	Auth       string       `json:"auth,omitempty"` // "api_key", "oauth" or "" (unknown)
	Agent      string       `json:"agent,omitempty"`
	At         int64        `json:"at"`
}

// KV is the subset of the store used here.
type KV interface {
	GetKV(key string) (string, bool, error)
	SetKV(key, val string) error
}

type rawWindow struct {
	Used     any `json:"used_percentage"`
	ResetsAt any `json:"resets_at"`
	UsedUSD  any `json:"used_usd"`
	LimitUSD any `json:"limit_usd"`
}

type rawInput struct {
	RateLimits struct {
		FiveHour   *rawWindow `json:"five_hour"`
		SevenDay   *rawWindow `json:"seven_day"`
		SpendLimit *rawWindow `json:"spend_limit"`
	} `json:"rate_limits"`
	SpendLimit *rawWindow `json:"spend_limit"`
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// window needs a numeric used percentage; otherwise the window is unknown.
func (r *rawWindow) window() *Window {
	if r == nil {
		return nil
	}
	p, ok := num(r.Used)
	if !ok {
		return nil
	}
	w := &Window{UsedPercent: p}
	if t, ok := num(r.ResetsAt); ok {
		w.ResetsAt = int64(t)
	}
	return w
}

func (r *rawWindow) spend() *SpendWindow {
	w := r.window()
	if w == nil {
		return nil
	}
	s := &SpendWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
	if v, ok := num(r.UsedUSD); ok {
		s.UsedUSD = &v
	}
	if v, ok := num(r.LimitUSD); ok {
		s.LimitUSD = &v
	}
	return s
}

// Parse extracts the rate limits from statusLine stdin JSON. Missing or
// non-numeric fields become nil. The error is non-nil only for invalid JSON;
// the returned snapshot is then empty but usable.
func Parse(stdin []byte) (Snapshot, error) {
	var in rawInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Snapshot{}, err
	}
	sp := in.RateLimits.SpendLimit
	if sp == nil {
		sp = in.SpendLimit
	}
	return Snapshot{
		FiveHour:   in.RateLimits.FiveHour.window(),
		SevenDay:   in.RateLimits.SevenDay.window(),
		SpendLimit: sp.spend(),
	}, nil
}

// Empty reports whether no window is known.
func (s Snapshot) Empty() bool {
	return s.FiveHour == nil && s.SevenDay == nil && s.SpendLimit == nil
}

// Same reports whether a and b carry the same limits and auth, ignoring Agent and At.
func Same(a, b Snapshot) bool {
	a.Agent, a.At, b.Agent, b.At = "", 0, "", 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// DetectAuth classifies the agent's credentials: "api_key" when ANTHROPIC_API_KEY
// is set, "oauth" when the Claude credentials file holds a subscription, else "".
func DetectAuth(getenv func(string) string, home string) string {
	if getenv("ANTHROPIC_API_KEY") != "" {
		return "api_key"
	}
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return ""
	}
	var c struct {
		OAuth struct {
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &c) == nil && c.OAuth.SubscriptionType != "" {
		return "oauth"
	}
	return ""
}

// Save stores snap as the current snapshot.
func Save(st KV, snap Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return st.SetKV(Key, string(b))
}

// Load returns the stored snapshot; ok is false when none has been saved.
func Load(st KV) (snap Snapshot, ok bool, err error) {
	v, found, err := st.GetKV(Key)
	if err != nil || !found {
		return Snapshot{}, false, err
	}
	if err := json.Unmarshal([]byte(v), &snap); err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, nil
}
