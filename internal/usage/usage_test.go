package usage

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceFor(t *testing.T) {
	cases := []struct {
		model string
		ov    Overrides
		want  Price
	}{
		{"claude-opus-5-5", nil, DefaultPrices["opus"]},
		{"claude-sonnet-5-5", nil, DefaultPrices["sonnet"]},
		{"claude-haiku-4-5", nil, DefaultPrices["haiku"]},
		{"fable-1", nil, DefaultPrices["fable"]},
		{"gpt-x", nil, FallbackPrice},
		{"claude-opus-5-5", Overrides{"opus": {Input: 1}}, Price{Input: 1}},
		{"claude-opus-5-5", Overrides{"claude-opus-5-5": {Input: 2}, "opus": {Input: 1}}, Price{Input: 2}},
	}
	for _, c := range cases {
		if got := PriceFor(c.model, c.ov); got != c.want {
			t.Errorf("%s: got %v want %v", c.model, got, c.want)
		}
	}
}

func TestCost(t *testing.T) {
	got := Cost(Tokens{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}, Price{1, 2, 3, 4})
	if !near(got, 10) {
		t.Fatalf("got %v", got)
	}
}

func TestParseTranscript(t *testing.T) {
	tot, err := ParseTranscript("testdata/transcript.jsonl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Messages != 2 || tot.Input != 12 || tot.Output != 150 || tot.CacheRead != 2000 || tot.CacheWrite != 1000 {
		t.Fatalf("%+v", tot)
	}
	want := Cost(Tokens{2, 100, 2000, 1000}, DefaultPrices["opus"]) + Cost(Tokens{10, 50, 0, 0}, DefaultPrices["sonnet"])
	if !near(tot.CostUSD, want) {
		t.Fatalf("cost %v want %v", tot.CostUSD, want)
	}
	if _, err := ParseTranscript("testdata/missing", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestRecordDelta(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src, _ := os.ReadFile("testdata/transcript.jsonl")
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tp, src, 0o644)

	d, err := Record(st, "a1", tp, nil)
	if err != nil || d.Prompt != 3012 || d.Output != 150 {
		t.Fatalf("first: %+v %v", d, err)
	}
	spend, _ := st.TodaySpend()
	if !near(spend, d.CostUSD) || spend <= 0 {
		t.Fatalf("spend %v", spend)
	}
	// unchanged transcript: nothing new
	if d, _ := Record(st, "a1", tp, nil); d != (Delta{}) {
		t.Fatalf("second: %+v", d)
	}
	// append a message: only the new one is counted
	f, _ := os.OpenFile(tp, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"message":{"id":"msg_4","model":"claude-sonnet-5-5","usage":{"input_tokens":5,"output_tokens":7}}}` + "\n")
	f.Close()
	d, _ = Record(st, "a1", tp, nil)
	if d.Prompt != 5 || d.Output != 7 {
		t.Fatalf("third: %+v", d)
	}
	// different transcript (new session) starts from zero
	tp2 := filepath.Join(t.TempDir(), "t2.jsonl")
	os.WriteFile(tp2, src, 0o644)
	if d, _ := Record(st, "a1", tp2, nil); d.Prompt != 3012 {
		t.Fatalf("new session: %+v", d)
	}
	// other agent independent
	if d, _ := Record(st, "a2", tp, nil); d.Output != 157 {
		t.Fatalf("a2: %+v", d)
	}
	if d, err := Record(st, "a1", "", nil); d != (Delta{}) || err != nil {
		t.Fatal("empty path")
	}
}
