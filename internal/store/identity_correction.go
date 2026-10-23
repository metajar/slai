package store

import (
	"encoding/json"
	"strconv"
	"time"
)

// KVIdentityCorrection + agent: unix time of the last identity correction queued for it.
const KVIdentityCorrection = "identity.correction."

// IdentityCorrectionEvery is the shortest gap between two corrections queued for one agent:
// a confused agent trips several signals at once, and one identity line is enough.
const IdentityCorrectionEvery = 10 * time.Minute

// IdentityMessageKind is the inbox kind of a correction; get_inbox lists it first.
const IdentityMessageKind = "identity"

// RecordIdentityCorrection notes that agent may have lost track of who it is (signal says how
// slai noticed, detail names the evidence). It always records an identity_correction event.
// Unless one was queued within IdentityCorrectionEvery it also queues identityLine plus an
// instruction to call whoami as an inbox item from slai, which get_inbox puts ahead of the
// rest and the next wake delivers. An empty identityLine records the event only.
func (s *Store) RecordIdentityCorrection(agent, signal, detail, identityLine string, now time.Time) (queued bool, err error) {
	last, _ := strconv.ParseInt(kvOrEmpty(s, KVIdentityCorrection+agent), 10, 64)
	queued = identityLine != "" && (last == 0 || now.Sub(time.Unix(last, 0)) >= IdentityCorrectionEvery)
	if queued {
		_, err = s.AddMessage(Message{From: "slai", To: agent, Kind: IdentityMessageKind,
			Body: identityLine + " slai noticed something that suggests you may have lost track of that (" + signal + ": " + detail +
				"). Call `whoami` now to confirm who you are and what you are for, then carry on as that agent."})
		if err != nil {
			return false, err
		}
		if err = s.SetKV(KVIdentityCorrection+agent, strconv.FormatInt(now.Unix(), 10)); err != nil {
			return true, err
		}
	}
	d, _ := json.Marshal(map[string]any{"agent": agent, "signal": signal, "detail": detail, "queued": queued})
	return queued, s.AddEvent(agent, "identity_correction", string(d))
}

func kvOrEmpty(s *Store, key string) string {
	v, _, _ := s.GetKV(key)
	return v
}
