package tui

import (
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/oorbt/slai/internal/tmux"
)

// returnKey reports whether k leaves focus mode (ctrl+q, ctrl+\). Esc, arrows
// and everything else go to the agent: Claude Code needs them.
func returnKey(k tea.KeyMsg) bool {
	return k.Type == tea.KeyCtrlQ || k.Type == tea.KeyCtrlBackslash
}

var namedKeys = map[tea.KeyType]string{
	tea.KeyEnter: "Enter", tea.KeyTab: "Tab", tea.KeyShiftTab: "BTab", tea.KeyBackspace: "BSpace",
	tea.KeyEsc: "Escape", tea.KeyUp: "Up", tea.KeyDown: "Down", tea.KeyLeft: "Left", tea.KeyRight: "Right",
	tea.KeyHome: "Home", tea.KeyEnd: "End", tea.KeyPgUp: "PgUp", tea.KeyPgDown: "PgDn",
	tea.KeyDelete: "DC", tea.KeyInsert: "IC",
	tea.KeyCtrlUp: "C-Up", tea.KeyCtrlDown: "C-Down", tea.KeyCtrlLeft: "C-Left", tea.KeyCtrlRight: "C-Right",
	tea.KeyShiftUp: "S-Up", tea.KeyShiftDown: "S-Down", tea.KeyShiftLeft: "S-Left", tea.KeyShiftRight: "S-Right",
	tea.KeyCtrlShiftUp: "C-S-Up", tea.KeyCtrlShiftDown: "C-S-Down", tea.KeyCtrlShiftLeft: "C-S-Left", tea.KeyCtrlShiftRight: "C-S-Right",
	tea.KeyCtrlHome: "C-Home", tea.KeyCtrlEnd: "C-End", tea.KeyShiftHome: "S-Home", tea.KeyShiftEnd: "S-End",
	tea.KeyCtrlShiftHome: "C-S-Home", tea.KeyCtrlShiftEnd: "C-S-End",
	tea.KeyCtrlPgUp: "C-PgUp", tea.KeyCtrlPgDown: "C-PgDn",
	tea.KeyCtrlAt: "C-Space", tea.KeyCtrlCloseBracket: "C-]", tea.KeyCtrlCaret: "C-^", tea.KeyCtrlUnderscore: "C-_",
}

// translateKey converts a bubbletea key press into tmux send-keys elements.
// paste is non-empty for a bracketed paste (send it with Client.Paste).
// ok is false for keys that cannot be forwarded.
func translateKey(k tea.KeyMsg) (keys []tmux.Key, paste string, ok bool) {
	if k.Paste {
		return nil, string(k.Runes), len(k.Runes) > 0
	}
	alt := ""
	if k.Alt {
		alt = "M-"
	}
	switch k.Type {
	case tea.KeyRunes:
		if len(k.Runes) == 0 {
			return nil, "", false
		}
		if k.Alt {
			// tmux M-<char> sends ESC + char; one key per rune keeps that exact.
			for _, r := range k.Runes {
				keys = append(keys, tmux.Named("M-"+string(r)))
			}
			return keys, "", true
		}
		return []tmux.Key{tmux.Lit(string(k.Runes))}, "", true
	case tea.KeySpace:
		if k.Alt {
			return []tmux.Key{tmux.Named("M-Space")}, "", true
		}
		return []tmux.Key{tmux.Lit(" ")}, "", true
	}
	if n, ok := namedKeys[k.Type]; ok {
		return []tmux.Key{tmux.Named(alt + n)}, "", true
	}
	if k.Type >= tea.KeyCtrlA && k.Type <= tea.KeyCtrlZ {
		letter := string(rune('a' + int(k.Type-tea.KeyCtrlA)))
		return []tmux.Key{tmux.Named(alt + "C-" + letter)}, "", true
	}
	if k.Type <= tea.KeyF1 && k.Type >= tea.KeyF20 {
		n := int(tea.KeyF1-k.Type) + 1
		return []tmux.Key{tmux.Named(alt + "F" + strconv.Itoa(n))}, "", true
	}
	return nil, "", false
}
