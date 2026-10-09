#!/usr/bin/env bash
# Smoke test for OpenClaw on Agent Substrate.
#
# Creates a session, asks OpenClaw to write a file, lets the actor suspend, then
# resumes it with a follow-up that only works if OpenClaw's session and its
# workspace came back. Prints the actor state between turns so you can watch it
# sleep. Needs: kagent CLI (pointed at the controller), kubectl, kubectl-ate, jq.
#
#   kubectl port-forward -n kagent svc/kagent-controller 8083:8083 &
#   ./scripts/smoke-test.sh [agent-name]
set -euo pipefail

AGENT="${1:-openclaw}"
WORD="substrate$RANDOM"
ATESPACE="${ATESPACE:-kagent}"

fail() { echo "FAIL: $*" >&2; exit 1; }

actor_state() {
  kubectl ate get actors --atespace "$ATESPACE" 2>/dev/null \
    | awk -v id="session-$SESSION_ID" '$2 == id {print $4, $5}'
}

wait_for_suspend() {
  for _ in $(seq 1 30); do
    state=$(actor_state)
    [[ "$state" == ACTOR_STATE_SUSPENDED* ]] && break
    sleep 2
  done
  echo "actor: ${state:-unknown}"
  [[ "${state:-}" == ACTOR_STATE_SUSPENDED* ]] || fail "actor did not suspend after the turn"
}

SESSION_ID=$(kagent agent session create --agent "$AGENT" -o json | jq -r '.session.id')
[ -n "$SESSION_ID" ] && [ "$SESSION_ID" != null ] || fail "could not create a session"
trap 'kagent agent session delete "$SESSION_ID" >/dev/null 2>&1 || true' EXIT
echo "session: $SESSION_ID"

echo "== turn 1: create a file"
kagent agent invoke --session "$SESSION_ID" \
  --task "Create $WORD.txt in your workspace containing the word $WORD. Reply in one short sentence." \
  || fail "turn 1 was not accepted (see TROUBLESHOOTING.md)"
wait_for_suspend

echo "== turn 2: resume, recall, and read the file back"
answer=$(kagent agent invoke --session "$SESSION_ID" \
  --task "Which file did I ask you to create? Read it and tell me what it contains, in one sentence.") \
  || fail "turn 2 was not accepted: the actor could not be resumed"
echo "$answer"
grep -qi "$WORD" <<<"$answer" || fail "OpenClaw did not recall the earlier turn or the file is gone"
wait_for_suspend

echo "PASS: suspend, resume, conversation restore, and file persistence all work"
