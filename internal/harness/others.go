package harness

import (
	"errors"

	"github.com/oorbt/slai/internal/tmux"
)

// ErrNotImplemented is returned by an adapter that has not landed yet. All three
// adapters have landed; it stays for future harnesses and the tests that use it.
var ErrNotImplemented = errors.New("harness adapter not implemented yet")

func plainKeys() KeySpec {
	return KeySpec{ClearSeq: []tmux.Key{tmux.Named("C-u")}, SubmitKey: tmux.Named("Enter"), InterruptSeq: []tmux.Key{tmux.Named("Escape")}}
}
