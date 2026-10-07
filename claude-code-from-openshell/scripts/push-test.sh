#!/usr/bin/env bash
# End-to-end test of the GitHub push flow: Claude pushes a file to a
# branch through the GitHub MCP server, you approve each call, and the commit lands.
#
# It rejects the first file write with a reason, lets Claude retry, and approves
# the retry. That is the "diagnose a denial and iterate" loop from the OpenShell
# tutorial. Run it only against a throwaway repo you own. Needs: kagent CLI,
# kubectl, jq, gh (to verify the commit), and the controller port-forward.
#
#   kubectl port-forward -n kagent svc/kagent-controller 8083:8083 &
#   ./scripts/push-test.sh <owner>/<repo>
set -euo pipefail

REPO="${1:?usage: push-test.sh <owner>/<repo>}"
AGENT="${AGENT:-claude}"
BRANCH="claude-hello-$RANDOM"
HERE="$(cd "$(dirname "$0")" && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }

SESSION_ID=$(kagent agent session create --agent "$AGENT" -o json | jq -r '.session.id')
[ -n "$SESSION_ID" ] && [ "$SESSION_ID" != null ] || fail "could not create a session"
trap 'kagent agent session delete "$SESSION_ID" >/dev/null 2>&1 || true' EXIT
echo "session: $SESSION_ID, branch: $BRANCH"

echo "== ask for a branch and a file"
kagent agent invoke --session "$SESSION_ID" \
  --task "In $REPO, create a branch called $BRANCH, then add hello_world.py (a one-line Python hello world) to it using the GitHub tools. Do not ask me for a token."

rejected=false
for _ in $(seq 1 40); do
  if gh api "repos/$REPO/contents/hello.py?ref=$BRANCH" --jq .path >/dev/null 2>&1; then
    echo "PASS: hello.py is on $BRANCH, pushed after one rejection and the approvals"
    exit 0
  fi
  if pending=$("$HERE/approve.sh" "$SESSION_ID" pending 2>/dev/null); then
    if [[ "$pending" == *create_or_update_file* || "$pending" == *push_files* ]] && ! $rejected; then
      echo "== reject the first file write"
      "$HERE/approve.sh" "$SESSION_ID" reject "Name the file hello.py, not hello_world.py"
      rejected=true
      sleep 10
      echo "== ask Claude to retry"
      kagent agent invoke --session "$SESSION_ID" \
        --task "Go ahead and push it again, using the file name from my rejection reason."
    else
      echo "== approve"
      "$HERE/approve.sh" "$SESSION_ID"
    fi
  fi
  sleep 3
done
fail "hello.py never appeared on $BRANCH"
