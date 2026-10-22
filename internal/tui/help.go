package tui

const helpText = `slai keys

AGENT LIST
  ↑ ↓ / j k    move (groups: ← / → fold, enter toggles)
  enter / →    FOCUS the agent: every key goes to its pane
               (Esc, arrows, ctrl+c all reach Claude)
  ctrl+q       leave focus, back to the list (also ctrl+\)
  space        quick prompt: type a line, enter sends it, esc cancels
               (tab completes task ids, slai/ branches, agent ids; shift+tab back; esc restores)
  c            redirect a busy agent: interrupt its turn (Escape) and send a course correction
  o            quick prompt to the orchestrator
  I            agent info: harness, model, degraded features, last 5 health events
  1-9          select the nth agent (its digit is on the row); again = attach
  n            new agent: fill a form, it is added to team.yaml
  g            jump to the next agent that needs you
  b            Tasks view (enter detail, d diff, + raise / - lower / T top priority, esc back)
  l            Log view (h toggles hook events, esc back)
  M            Team memory: facts with state (fresh / stale / unverified / invalid), f filters, enter detail, e edit, d delete (asks first)
  s            Stats view: merged, review rounds, rework %, refusals, $/merged task per agent and per harness/model (esc back)
  t / d        task detail / diff of the selected agent's task
  v            revive a dead agent (claude --continue)
  x            stop the selected agent's session (asks first)
  R            restart with a fresh conversation (asks first)
  a            full-screen tmux attach (ctrl+q detaches back)
  i            usage details (limits, reset times, spend, source)
  U            show / hide the usage strip (5h / weekly limits) in the header
  p            pause / resume the team (stops wake-up delivery)
  H            hold the team (asks for an optional name; nothing is woken, nudged or restarted, sessions stay up); H again resumes
  ?            this help       q   quit (agents keep running)

MOUSE  click an agent: select · click the preview: focus
       wheel over the list: scroll the list
       wheel over the preview depends on the agent's screen:
         app handles the mouse (Claude Code): sent to it as mouse reports, so it
           scrolls itself and its "jump to bottom" button works (focused click
           is forwarded too; right/middle as well)
         alternate screen, no mouse: Up/Down keys (3 per notch)
         normal screen: scrolls the tmux history (3 lines/notch, "N lines
           back"); PgUp/PgDn/Home/End page it; typing returns to the bottom
       drag in the preview (never forwarded): select + copy (clipboard + OSC 52);
         double/triple click = word/line when the app has no mouse handling
         (otherwise they are forwarded clicks); esc / any key clears it
  m    (list) toggle mouse capture: off = your terminal's own selection and
       scrolling (shift+drag also works in most terminals); the footer shows the state.
       The preview has no side bars, so a native selection copies only agent text

Glyphs: ● idle/working   ▲ needs you (amber)   ◆ review/done   ○ dead

press any key to close`
