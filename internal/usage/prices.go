// Package usage prices token usage and parses Claude Code transcripts.
package usage

import "strings"

// Price is dollars per million tokens.
type Price struct {
	Input, Output, CacheRead, CacheWrite float64
}

// ---------------------------------------------------------------------------
// EDITABLE PRICE TABLE ($ per million tokens), keyed by model family.
// Cache read ~= 0.1x input, cache write (5m) ~= 1.25x input.
// Update when Anthropic changes pricing; users can also pass overrides.
// ---------------------------------------------------------------------------
var DefaultPrices = map[string]Price{
	"opus":   {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"sonnet": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"haiku":  {Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25},
	"fable":  {Input: 10, Output: 50, CacheRead: 1.00, CacheWrite: 12.50}, // estimate
}

// FallbackPrice is used for unknown models (priced like sonnet).
var FallbackPrice = Price{Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75}

// Overrides maps an exact model id or a family name to a replacement price.
type Overrides map[string]Price

// Families is the match order for family lookup.
var Families = []string{"opus", "sonnet", "haiku", "fable"}

// PriceFor resolves a model id: exact override, family override, default
// family price, then FallbackPrice.
func PriceFor(model string, ov Overrides) Price {
	m := strings.ToLower(model)
	if p, ok := ov[m]; ok {
		return p
	}
	for _, f := range Families {
		if strings.Contains(m, f) {
			if p, ok := ov[f]; ok {
				return p
			}
			return DefaultPrices[f]
		}
	}
	return FallbackPrice
}

// Tokens is a usage tally.
type Tokens struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Prompt is all input-side tokens (fresh + cache read + cache write).
func (t Tokens) Prompt() int64 { return t.Input + t.CacheRead + t.CacheWrite }

// Cost returns USD for the tally at price p.
func Cost(t Tokens, p Price) float64 {
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead + float64(t.CacheWrite)*p.CacheWrite) / 1e6
}
