#!/bin/bash
# Ralph loop — pipe PROMPT.md into an AI agent on repeat
# Canonical version: ~/.claude/scripts/ralph.sh
# Copy into project roots when setting up ralph loops.
# Ctrl+C to stop
#
# Usage:
#   ./ralph.sh [loop_name] [agent] [model]
#
#   loop_name   Which PROMPT to use (optional). No arg or "-" = PROMPT.md, with arg = PROMPT-{name}.md
#   agent       Which AI CLI to use: claude (default), codex
#   model       Model override (e.g. spark, o3, sonnet). Passed as -m/--model to the agent.
#
# Environment overrides:
#   RALPH_AGENT_CMD   Full command override (e.g. "codex exec --yolo -")
#
# Examples:
#   ./ralph.sh                        # PROMPT.md with Claude
#   ./ralph.sh core                   # PROMPT-core.md with Claude
#   ./ralph.sh core codex spark       # PROMPT-core.md with Codex using spark model
#   ./ralph.sh - codex spark          # PROMPT.md with Codex using spark model
#
show_help() {
  cat <<'HELP'
ralph — autonomous agent loop runner

Pipes a PROMPT file into an AI agent repeatedly. Each iteration the agent
reads the prompt, picks the next work item, implements it, and commits.
Loop until you Ctrl+C.

USAGE
  ./ralph.sh [loop_name] [agent] [model]
  ./ralph.sh -h | --help

ARGUMENTS
  loop_name   Which PROMPT file to use (optional)
              No arg or "-" → PROMPT.md, with arg → PROMPT-{loop_name}.md
              Common values: core, brain, integration

  agent       Which AI CLI to run (default: claude)
              Built-in options: claude, codex
              Or set RALPH_AGENT_CMD for custom agents.

  model       Model override (optional)
              Passed as -m/--model to the agent CLI.
              Examples: spark, o3, sonnet, opus

ENVIRONMENT
  RALPH_AGENT_CMD   Override the full agent command.
                    Example: RALPH_AGENT_CMD="codex exec --yolo -"

EXAMPLES
  ./ralph.sh                       # Run PROMPT.md with Claude
  ./ralph.sh - codex               # Run PROMPT.md with Codex
  ./ralph.sh core codex spark      # Run PROMPT-core.md with Codex (spark model)
  ./ralph.sh - codex spark         # Run PROMPT.md with Codex (spark model)
  ./ralph.sh core claude sonnet    # Run PROMPT-core.md with Claude (sonnet model)

SIGNALS
  Ctrl+C once    Let current iteration finish, then stop
  Ctrl+C twice   Kill immediately

FILES
  PROMPT-{name}.md    Agent instructions (required, in project root)
  progress-{name}.md  State tracker (read/written by agent)
  ralph-{name}.log    Append-only log of all iterations

HELP
  exit 0
}

# Handle --help / -h before anything else
case "${1:-}" in -h|--help) show_help ;; esac

set +e  # don't exit on failures — agent may exit non-zero on API blips, timeouts, etc.
set -m  # job control: background jobs get their own process group, shielded from Ctrl+C

cd "$(dirname "$0")"

# Determine which PROMPT file to use
# No arg or "-" → PROMPT.md, with arg → PROMPT-{arg}.md
LOOP_NAME="${1:-}"
if [ -z "$LOOP_NAME" ] || [ "$LOOP_NAME" = "-" ]; then
  PROMPT_FILE="PROMPT.md"
  LOOP_NAME="default"
else
  PROMPT_FILE="PROMPT-${LOOP_NAME}.md"
fi

# Verify PROMPT file exists
if [ ! -f "$PROMPT_FILE" ]; then
  echo "Error: $PROMPT_FILE not found in $(pwd)"
  exit 1
fi

# Agent selection: explicit env var > second arg > default (claude)
AGENT_NAME="${2:-claude}"
MODEL="${3:-}"
if [ -n "$RALPH_AGENT_CMD" ]; then
  AGENT_CMD="$RALPH_AGENT_CMD"
elif [ "$AGENT_NAME" = "claude" ]; then
  AGENT_CMD="$HOME/.local/bin/claude -p --dangerously-skip-permissions"
elif [ "$AGENT_NAME" = "codex" ]; then
  AGENT_CMD="codex exec --yolo"
else
  echo "Error: Unknown agent '$AGENT_NAME'. Use 'claude', 'codex', or set RALPH_AGENT_CMD."
  exit 1
fi

# Append model flag if specified
if [ -n "$MODEL" ] && [ -z "$RALPH_AGENT_CMD" ]; then
  AGENT_CMD="$AGENT_CMD -m $MODEL"
fi

# Verify the agent binary exists before looping
AGENT_BIN="${AGENT_CMD%% *}"
if ! command -v "$AGENT_BIN" &>/dev/null && [ ! -x "$AGENT_BIN" ]; then
  echo "Error: Agent binary not found: $AGENT_BIN"
  echo "Install it or set RALPH_AGENT_CMD to the correct path."
  exit 1
fi

# Two-tier Ctrl+C handling:
#   1st Ctrl+C → let current iteration finish naturally, then stop the loop
#   2nd Ctrl+C → kill the current iteration immediately and exit
#
# The child runs in a background process group (set -m), so terminal SIGINT
# (Ctrl+C) only reaches bash — not the agent. `wait` is interruptible by signals
# when job control is active, so the trap fires immediately.
STOP_COUNT=0
CHILD_PID=""

trap '
  STOP_COUNT=$((STOP_COUNT + 1))
  if [ $STOP_COUNT -ge 2 ]; then
    echo "" | tee -a "$LOGFILE"
    echo "[$(date "+%Y-%m-%d %H:%M:%S")] Force kill. Exiting now." | tee -a "$LOGFILE"
    [ -n "$CHILD_PID" ] && kill -- -$CHILD_PID 2>/dev/null
    exit 1
  fi
  echo "" | tee -a "$LOGFILE"
  echo "[$(date "+%Y-%m-%d %H:%M:%S")] Ctrl+C received. Will stop after current iteration completes. (Ctrl+C again to kill now)" | tee -a "$LOGFILE"
' INT

# Disable SSH askpass GUI popup — piped stdin confuses SSH into thinking
# there's no terminal, so it launches the askpass dialog even for keys
# with no passphrase. Unsetting this forces direct key file usage.
unset SSH_ASKPASS

LOGFILE="ralph-${LOOP_NAME}.log"
ITERATION=0

echo "Starting ralph loop for $LOOP_NAME using $PROMPT_FILE..."
echo "Agent: $AGENT_CMD"
echo "Model: ${MODEL:-default}"
echo "Press Ctrl+C to stop (once = after iteration, twice = immediate)."
echo "---"

while :; do
  if [ $STOP_COUNT -ge 1 ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ralph loop stopped cleanly." | tee -a "$LOGFILE"
    exit 0
  fi

  ITERATION=$((ITERATION + 1))
  START=$(date +%s)
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] iteration $ITERATION started ($AGENT_NAME)" | tee -a "$LOGFILE"

  # Run in background process group so Ctrl+C doesn't kill the agent.
  # stdin redirected from /dev/null to prevent SIGTTIN (background read from terminal).
  #
  # script(1) provides a PTY so the agent thinks it has a terminal.
  # macOS and Linux have different syntax:
  #   Linux: script -qec "command" /dev/null
  #   macOS: script -q /dev/null command args...
  if [[ "$OSTYPE" == "darwin"* ]]; then
    SCRIPT_CMD="script -q /dev/null"
  else
    SCRIPT_CMD="script -qec"
  fi

  if [ "$AGENT_NAME" = "codex" ]; then
    # Codex dumps verbose output (thinking, tool calls, input echo) to stdout.
    # Suppress script output entirely, use -o to capture only the final clean
    # response, then cat that into the log.
    CODEX_OUT=$(mktemp /tmp/ralph-codex-XXXXXX.txt)
    if [[ "$OSTYPE" == "darwin"* ]]; then
      ($SCRIPT_CMD sh -c "cat $PROMPT_FILE | $AGENT_CMD -o $CODEX_OUT -" < /dev/null > /dev/null 2>&1; cat "$CODEX_OUT" | tee -a "$LOGFILE"; rm -f "$CODEX_OUT") &
    else
      (script -qec "cat $PROMPT_FILE | $AGENT_CMD -o $CODEX_OUT -" /dev/null < /dev/null > /dev/null 2>&1; cat "$CODEX_OUT" | tee -a "$LOGFILE"; rm -f "$CODEX_OUT") &
    fi
  else
    # Claude: pipe script output through tee into the log.
    if [[ "$OSTYPE" == "darwin"* ]]; then
      ($SCRIPT_CMD sh -c "cat $PROMPT_FILE | $AGENT_CMD" < /dev/null 2>&1 | tee -a "$LOGFILE") &
    else
      (script -qec "cat $PROMPT_FILE | $AGENT_CMD" /dev/null < /dev/null 2>&1 | tee -a "$LOGFILE") &
    fi
  fi
  CHILD_PID=$!

  # Wait for child to finish. Re-wait if interrupted by signal (Ctrl+C)
  # since the child is still running and we want it to complete.
  while kill -0 $CHILD_PID 2>/dev/null; do
    wait $CHILD_PID 2>/dev/null
  done
  wait $CHILD_PID 2>/dev/null
  EXIT_CODE=$?
  CHILD_PID=""

  ELAPSED=$(( $(date +%s) - START ))
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] iteration $ITERATION finished (exit=$EXIT_CODE, ${ELAPSED}s)" | tee -a "$LOGFILE"
  echo "---" | tee -a "$LOGFILE"
done
