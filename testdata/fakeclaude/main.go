// Command fakeclaude is a stand-in for the `claude` binary used in slai
// integration tests. It records its flags to $FAKECLAUDE_LOG (JSON lines),
// prints a banner, and echoes stdin lines until EOF or "exit".
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type record struct {
	Time  string            `json:"time"`
	Event string            `json:"event"`
	Args  []string          `json:"args,omitempty"`
	Flags map[string]string `json:"flags,omitempty"`
	Cwd   string            `json:"cwd,omitempty"`
	Agent string            `json:"slai_agent_id,omitempty"`
	Proj  string            `json:"slai_project,omitempty"`
	Line  string            `json:"line,omitempty"`
}

var valueFlags = map[string]bool{
	"--model": true, "--append-system-prompt": true, "--mcp-config": true,
	"--settings": true, "--permission-mode": true,
	"--session-id": true, "--resume": true,
}
var boolFlags = map[string]bool{"--continue": true, "-c": true}

func logRec(f *os.File, r record) {
	if f == nil {
		return
	}
	r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
}

// conversationFile is the state file recording that a conversation exists for
// this agent+cwd: under $FAKECLAUDE_STATE, else the temp dir.
func conversationFile(cwd string) string {
	if sid := os.Getenv("FAKECLAUDE_SESSION"); sid != "" {
		base := os.Getenv("FAKECLAUDE_STATE")
		if base == "" {
			base = filepath.Join(os.TempDir(), "fakeclaude-state")
		}
		return filepath.Join(base, "session-"+sid)
	}
	base := os.Getenv("FAKECLAUDE_STATE")
	if base == "" {
		base = filepath.Join(os.TempDir(), "fakeclaude-state")
	}
	sum := sha1.Sum([]byte(cwd + "|" + os.Getenv("SLAI_PROJECT")))
	return filepath.Join(base, os.Getenv("SLAI_AGENT_ID")+"-"+hex.EncodeToString(sum[:4]))
}

func main() {
	var lf *os.File
	if p := os.Getenv("FAKECLAUDE_LOG"); p != "" {
		lf, _ = os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		defer lf.Close()
	}
	args := os.Args[1:]
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasEq := strings.Cut(a, "=")
		switch {
		case valueFlags[name]:
			if !hasEq && i+1 < len(args) {
				i++
				val = args[i]
			}
			flags[name] = val
		case boolFlags[a]:
			flags["--continue"] = "true"
		}
	}
	cwd, _ := os.Getwd()
	logRec(lf, record{Event: "start", Args: args, Flags: flags, Cwd: cwd,
		Agent: os.Getenv("SLAI_AGENT_ID"), Proj: os.Getenv("SLAI_PROJECT")})

	if os.Getenv("FAKECLAUDE_NO_CONVERSATION_ON_CONTINUE") == "1" {
		// Behave like real claude. --continue/--resume fail when there is no earlier
		// conversation; --session-id fails when that session already exists.
		sid := flags["--resume"]
		if sid == "" {
			sid = flags["--session-id"]
		}
		if sid != "" {
			os.Setenv("FAKECLAUDE_SESSION", sid)
		}
		f := conversationFile(cwd)
		_, statErr := os.Stat(f)
		fail := func(msg string) {
			fmt.Println(msg)
			logRec(lf, record{Event: "exit"})
			if lf != nil {
				lf.Close()
			}
			os.Exit(1)
		}
		switch {
		case flags["--resume"] != "" && statErr != nil:
			fail("No conversation found with session ID: " + flags["--resume"])
		case flags["--session-id"] != "" && statErr == nil:
			fail("Error: Session ID " + flags["--session-id"] + " is already in use.")
		case flags["--continue"] != "" && statErr != nil:
			fail("No conversation found to continue")
		}
		_ = os.MkdirAll(filepath.Dir(f), 0o755)
		_ = os.WriteFile(f, []byte("x\n"), 0o644)
	}

	if autopilotEnabled() {
		runAutopilot(lf, flags)
		logRec(lf, record{Event: "exit"})
		return
	}

	fmt.Println("fakeclaude 0.0.0 (test stand-in for Claude Code)")
	fmt.Printf("model: %s\n", flags["--model"])
	if flags["--continue"] != "" || flags["--resume"] != "" {
		fmt.Println("Continuing previous conversation")
	}
	fmt.Println("> ready")

	switch os.Getenv("FAKECLAUDE_ALT_MOUSE") {
	case "1":
		runAlt(true)
		return
	case "keys":
		runAlt(false)
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		logRec(lf, record{Event: "input", Line: line})
		if strings.TrimSpace(line) == "exit" {
			fmt.Println("bye")
			break
		}
		if t := strings.TrimSpace(line); t == "@altmouse" || t == "@altkeys" {
			runAlt(t == "@altmouse")
			break
		}
		fmt.Printf("echo: %s\n> ", line)
	}
	logRec(lf, record{Event: "exit"})
}
