package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// Totals is the cumulative usage found in one transcript.
type Totals struct {
	Tokens
	CostUSD  float64
	Messages int
	// Last is the final assistant message's usage: its prompt side (Last.Prompt()) is how big
	// the conversation was when the agent last answered, i.e. its context size.
	Last Tokens
}

type line struct {
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
			CR  int64 `json:"cache_read_input_tokens"`
			CC  int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseTranscript sums assistant usage in a Claude Code transcript JSONL.
// Claude writes one line per content block with the same message id and
// repeated usage, so usage is de-duplicated by message id (last wins).
// Synthetic messages and malformed lines are skipped.
func ParseTranscript(path string, ov Overrides) (Totals, error) {
	f, err := os.Open(path)
	if err != nil {
		return Totals{}, err
	}
	defer f.Close()
	return ParseReader(f, ov)
}

// ParseReader is ParseTranscript over an io.Reader.
func ParseReader(r io.Reader, ov Overrides) (Totals, error) {
	type entry struct {
		t     Tokens
		model string
	}
	byID := map[string]entry{}
	var order []string
	anon := 0
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 && bytes.Contains(b, []byte(`"usage"`)) {
			var l line
			if json.Unmarshal(b, &l) == nil && l.Message.Usage != nil && l.Message.Model != "<synthetic>" {
				u := l.Message.Usage
				id := l.Message.ID
				if id == "" {
					anon++
					id = "anon-" + strconv.Itoa(anon)
				}
				if _, seen := byID[id]; !seen {
					order = append(order, id)
				}
				byID[id] = entry{Tokens{u.In, u.Out, u.CR, u.CC}, l.Message.Model}
			}
		}
		if err != nil {
			if err != io.EOF {
				return Totals{}, err
			}
			break
		}
	}
	var tot Totals
	for _, id := range order {
		e := byID[id]
		tot.Input += e.t.Input
		tot.Output += e.t.Output
		tot.CacheRead += e.t.CacheRead
		tot.CacheWrite += e.t.CacheWrite
		tot.CostUSD += Cost(e.t, PriceFor(e.model, ov))
		tot.Messages++
		tot.Last = e.t
	}
	return tot, nil
}

// MarkKind is the events kind used to remember how much of a transcript has
// already been billed (no extra table needed).
const MarkKind = "usage:mark"

type mark struct {
	Transcript string  `json:"transcript"`
	Prompt     int64   `json:"prompt"`
	Output     int64   `json:"output"`
	Cost       float64 `json:"cost"`
}

// Delta is what Record added.
type Delta struct {
	Prompt, Output int64
	CostUSD        float64
}

// Record parses the transcript and adds only the not-yet-recorded usage for
// agent via store.AddUsage (input_tokens = all prompt-side tokens incl. cache).
// Progress is remembered per agent+transcript in a "usage:mark" event, so
// repeated Stop hooks never double count and a new session (new transcript)
// starts from zero. Totals that shrink are ignored.
func Record(st *store.Store, agent, transcript string, ov Overrides) (Delta, error) {
	if transcript == "" {
		return Delta{}, nil
	}
	tot, err := ParseTranscript(transcript, ov)
	if err != nil {
		return Delta{}, err
	}
	var prev mark
	evs, err := st.EventsByKind(agent, MarkKind, 50)
	if err != nil {
		return Delta{}, err
	}
	for _, e := range evs {
		var m mark
		if json.Unmarshal([]byte(e.Data), &m) == nil && m.Transcript == transcript {
			prev = m
			break
		}
	}
	if tot.Messages > 0 {
		_ = SetContext(st, agent, tot.Last.Prompt())
	}
	cur := mark{transcript, tot.Prompt(), tot.Output, tot.CostUSD}
	d := Delta{cur.Prompt - prev.Prompt, cur.Output - prev.Output, cur.Cost - prev.Cost}
	if d.Prompt <= 0 && d.Output <= 0 && d.CostUSD <= 1e-12 {
		return Delta{}, nil
	}
	d.Prompt, d.Output = max(d.Prompt, 0), max(d.Output, 0)
	d.CostUSD = max(d.CostUSD, 0)
	if err := st.AddUsage(agent, d.Prompt, d.Output, d.CostUSD); err != nil {
		return Delta{}, err
	}
	b, _ := json.Marshal(cur)
	return d, st.AddEvent(agent, MarkKind, string(b))
}

// KVContext + agent holds "tokens|unix": the prompt size of the agent's latest turn (its
// context size) and when it was recorded. A new session's first turn overwrites it.
const KVContext = "usage.ctx."

// SetContext records the agent's latest-turn prompt size.
func SetContext(st *store.Store, agent string, tokens int64) error {
	return st.SetKV(KVContext+agent, strconv.FormatInt(tokens, 10)+"|"+strconv.FormatInt(time.Now().Unix(), 10))
}

// Context reads it back: tokens, when recorded (unix), and whether there is one.
func Context(st *store.Store, agent string) (tokens, at int64, ok bool) {
	v, found, err := st.GetKV(KVContext + agent)
	if err != nil || !found {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(v, "|")
	tokens, err1 := strconv.ParseInt(a, 10, 64)
	at, _ = strconv.ParseInt(b, 10, 64)
	return tokens, at, err1 == nil
}
