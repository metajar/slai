package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Test hook: SLAI_TEST_TIMESCALE (e.g. 0.02) together with SLAI_E2E=1 shrinks
// the health and stuck-detection thresholds by that factor so end-to-end tests
// can watch a self-heal in seconds instead of minutes. Both variables are
// required so a stray export cannot shrink a real deployment's thresholds, and
// applying it is announced on stderr. Each scaled value is clamped to 1s because
// the persisted timestamps it is compared with have second resolution. Counts,
// service.SupervisorDownAfter and the MCP stamp window are never scaled.
func init() {
	f, err := strconv.ParseFloat(os.Getenv("SLAI_TEST_TIMESCALE"), 64)
	if err != nil || f <= 0 || f >= 1 || os.Getenv("SLAI_E2E") != "1" {
		return
	}
	for _, d := range []*time.Duration{
		&StuckAfter, &StuckPaneGap, &StuckRestartAfter,
		&RateLimitProbe, &RateLimitGrace, &ModalPatience, &ModalRepeat,
		&UnreadAfter, &UnreadRestartAfter, &OrchSilentAfter, &WakeLandWindow,
	} {
		*d = max(time.Duration(float64(*d)*f), time.Second)
	}
	fmt.Fprintf(os.Stderr, "slai: SLAI_TEST_TIMESCALE=%v applied to supervisor thresholds (test mode)\n", f)
}
