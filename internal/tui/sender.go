package tui

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oorbt/slai/internal/tmux"
)

// paneIO is the slice of *tmux.Client the TUI uses; tests substitute a fake.
type paneIO interface {
	SendSeq(ctx context.Context, session string, keys []tmux.Key) error
	Paste(ctx context.Context, session, text string) error
	SendKeys(ctx context.Context, session, text string, enter bool) error
	Snapshot(ctx context.Context, session string) (tmux.Snapshot, error)
	ResizeWindow(ctx context.Context, session string, w, h int) error
	WindowSizeLatest(ctx context.Context, session string) error
	BindDetachKey(ctx context.Context, key string) error
	KillSession(ctx context.Context, name string) error
}

// scrollIO is implemented by pane clients that can capture scrollback windows.
type scrollIO interface {
	SnapshotAt(ctx context.Context, session string, offset int) (tmux.Snapshot, error)
}

// sendItem is one unit of input for an agent pane.
type sendItem struct {
	session string
	keys    []tmux.Key
	paste   string
}

// sender delivers key input to tmux strictly in order on one goroutine, so
// fast typing is never reordered and Update never blocks on a tmux exec.
type sender struct {
	io   paneIO
	q    chan sendItem
	wg   sync.WaitGroup
	once sync.Once
	err  atomic.Value // string: last delivery error
}

func newSender(io paneIO) *sender { return &sender{io: io, q: make(chan sendItem, 4096)} }

func (s *sender) enqueue(it sendItem) {
	s.once.Do(func() { go s.loop() })
	s.wg.Add(1)
	select {
	case s.q <- it:
	default:
		s.wg.Done() // queue full: drop rather than block the UI
	}
}

// wait blocks until everything enqueued so far was delivered (tests).
func (s *sender) wait() { s.wg.Wait() }

func (s *sender) lastErr() string {
	v, _ := s.err.Load().(string)
	return v
}

func (s *sender) loop() {
	for it := range s.q {
		// Merge following key items for the same session into one tmux call.
		batch := it
		var pending []sendItem
		for batch.paste == "" {
			select {
			case nx := <-s.q:
				if nx.session == batch.session && nx.paste == "" {
					batch.keys = append(batch.keys, nx.keys...)
					s.wg.Done()
					continue
				}
				pending = append(pending, nx)
			default:
			}
			break
		}
		s.deliver(batch)
		for _, p := range pending {
			s.deliver(p)
		}
	}
}

func (s *sender) deliver(it sendItem) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if it.paste != "" {
		err = s.io.Paste(ctx, it.session, it.paste)
	} else {
		err = s.io.SendSeq(ctx, it.session, it.keys)
	}
	if err != nil {
		s.err.Store(firstLine(err.Error()))
	} else {
		s.err.Store("")
	}
}
