#!/usr/bin/env bash
# Smoke test for the Pi agent on Agent Substrate.
#
# Runs the Step 6 flow from the post: create a session, give Pi a task, let the
# actor suspend, then resume it with a follow-up that only works if the adapter
# restored Pi's conversation. Prints the actor state between turns so you can
# watch it sleep. Needs: kagent CLI (pointed at the controller), kubectl,
# kubectl-ate, jq.
#
#   kubectl port-forward -n kagent svc/kagent-controller 8083:8083 &
#   ./scripts/smoke-test.sh [agent-name]
set -euo pipefail

AGENT="${1:-pi}"
WORD="substrate$RANDOM"
ATESPACE="${ATESPACE:-kagent}"

fail() { echo "FAIL: $*" >&2; exit 1; }

actor_state() {
  kubectl ate get actors --atespace "$ATESPACE" 2>/dev/null \
    | awk -v id="session-$SESSION_ID" '$2 == id {print $4, $5}'
}

SESSION_ID=$(kagent agent session create --agent "$AGENT" -o json | jq -r '.session.id')
[ -n "$SESSION_ID" ] && [ "$SESSION_ID" != null ] || fail "could not create a session"
trap 'kagent agent session delete "$SESSION_ID" >/dev/null 2>&1 || true' EXIT
echo "session: $SESSION_ID"

echo "== turn 1: create a file"
kagent agent invoke --session "$SESSION_ID" \
  --task "Create $WORD.txt containing the word $WORD. Reply in one short sentence." \
  || fail "turn 1 was not accepted (see Troubleshooting in the post)"

# The actor suspends a few seconds after the turn settles.
for _ in $(seq 1 15); do
  state=$(actor_state)
  [[ "$state" == ACTOR_STATE_SUSPENDED* ]] && break
  sleep 2
done
echo "actor after turn 1: ${state:-unknown}"
[[ "${state:-}" == ACTOR_STATE_SUSPENDED* ]] || fail "actor did not suspend after the turn"

echo "== turn 2: resume and recall"
answer=$(kagent agent invoke --session "$SESSION_ID" \
  --task "Which file did I ask you to create, and what word does it contain? One sentence.") \
  || fail "turn 2 was not accepted: the actor could not be resumed"
echo "$answer"
grep -qi "$WORD" <<<"$answer" || fail "Pi did not recall the earlier turn (is /data/adapter/pi-session written?)"

echo "actor after turn 2: $(actor_state)"
echo "PASS: suspend, resume, and conversation restore all work"
