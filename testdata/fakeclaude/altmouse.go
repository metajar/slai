package main

// Alt-screen mode (FAKECLAUDE_ALT_MOUSE=1|keys, or the input line "@altmouse" /
// "@altkeys"): like the real Claude Code, switch to the alternate screen and,
// for "mouse", turn on SGR mouse reporting. Every report / key received on
// raw stdin is decoded and printed, e.g. "MOUSE wheel-up col=12 row=7",
// "MOUSE press left col=3 row=4", "KEY Up".

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func runAlt(mouse bool) {
	st := exec.Command("stty", "raw", "-echo")
	st.Stdin = os.Stdin
	_ = st.Run()
	fmt.Print("\x1b[?1049h\x1b[2J\x1b[H")
	if mouse {
		fmt.Print("\x1b[?1000h\x1b[?1006h")
	}
	fmt.Print("ALT SCREEN READY\r\n")
	buf := make([]byte, 0, 64)
	one := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(one)
		if err != nil {
			return
		}
		buf = append(buf, one[:n]...)
		for len(buf) > 0 {
			ev, used := decodeInput(buf)
			if used == 0 {
				break // incomplete sequence
			}
			buf = buf[used:]
			if ev == "KEY ctrl-c" {
				return
			}
			fmt.Print(ev + "\r\n")
		}
	}
}

// decodeInput decodes one event from the head of b; used==0 means "need more bytes".
func decodeInput(b []byte) (string, int) {
	if b[0] != 0x1b {
		if b[0] == 3 {
			return "KEY ctrl-c", 1
		}
		return fmt.Sprintf("KEY %q", string(b[:1])), 1
	}
	if len(b) < 2 {
		return "", 0
	}
	if b[1] == '[' && len(b) >= 3 && b[2] == '<' {
		for i := 3; i < len(b); i++ {
			if b[i] == 'M' || b[i] == 'm' {
				var code, x, y int
				fmt.Sscanf(string(b[3:i]), "%d;%d;%d", &code, &x, &y)
				return decodeMouse(code, x, y, b[i] == 'M'), i + 1
			}
		}
		return "", 0
	}
	if b[1] == '[' && len(b) >= 3 {
		names := map[byte]string{'A': "Up", 'B': "Down", 'C': "Right", 'D': "Left", 'H': "Home", 'F': "End"}
		if nm, ok := names[b[2]]; ok {
			return "KEY " + nm, 3
		}
		if b[2] == '5' || b[2] == '6' {
			if len(b) >= 4 {
				return "KEY " + map[byte]string{'5': "PgUp", '6': "PgDn"}[b[2]], 4
			}
			return "", 0
		}
	}
	return "KEY esc", 1
}

func decodeMouse(code, x, y int, press bool) string {
	btn := []string{"left", "middle", "right"}
	var what string
	switch {
	case code&64 != 0:
		what = "wheel-up"
		if code&1 != 0 {
			what = "wheel-down"
		}
		return fmt.Sprintf("MOUSE %s col=%d row=%d", what, x, y)
	case code&32 != 0:
		what = "motion"
	case press:
		what = "press"
	default:
		what = "release"
	}
	b := "none"
	if code&3 < 3 {
		b = btn[code&3]
	}
	return strings.TrimSpace(fmt.Sprintf("MOUSE %s %s col=%d row=%d", what, b, x, y))
}
