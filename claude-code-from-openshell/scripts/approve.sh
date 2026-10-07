#!/usr/bin/env bash
# Answer a pending tool-approval request on a kagent Claude session.
#
# A Claude agent whose MCP binding sets requireApproval pauses the task in
# INPUT_REQUIRED before each tool call. The kagent UI shows Approve and Reject
# buttons; this script does the same over A2A JSON-RPC, so you can script it.
# Needs: curl, jq, and the controller port-forward from the README.
#
#   kubectl port-forward -n kagent svc/kagent-controller 8083:8083 &
#   ./scripts/approve.sh <session-id>                      # approve
#   ./scripts/approve.sh <session-id> reject "wrong repo"  # reject, with a reason
#   ./scripts/approve.sh <session-id> pending              # list pending calls, change nothing
set -euo pipefail

SESSION_ID="${1:?usage: approve.sh <session-id> [approve|reject|pending] [reason]}"
DECISION="${2:-approve}"
REASON="${3:-rejected by approve.sh}"
AGENT="${AGENT:-claude}"
NAMESPACE="${NAMESPACE:-kagent}"
API="${KAGENT_API:-http://localhost:8083}"
USER_ID="${KAGENT_USER_ID:-admin@kagent.dev}"
URI="https://kagent.dev/extensions/hitl/v1"

rpc() {
  curl -fsS -X POST "$API/agents/$NAMESPACE/$AGENT" \
    -H "X-User-Id: $USER_ID" -H "A2A-Extensions: $URI" \
    -H 'Content-Type: application/json' -d "$1"
}

task=$(rpc "$(jq -n --arg c "$SESSION_ID" \
  '{jsonrpc:"2.0",id:"list",method:"ListTasks",params:{contextId:$c}}')" \
  | jq -c '[.result.tasks[] | select(.status.state == "TASK_STATE_INPUT_REQUIRED")][0] // empty')
[ -n "$task" ] || { echo "no pending approval in session $SESSION_ID" >&2; exit 1; }

task_id=$(jq -r '.id' <<<"$task")
echo "pending on task $task_id:"
jq -r --arg u "$URI" '.status.message.metadata[$u].tools[] | "  \(.name) \(.args | tojson)"' <<<"$task"
[ "$DECISION" != pending ] || exit 0

approvals=$(jq -c --arg u "$URI" --arg d "$DECISION" --arg r "$REASON" \
  '[.status.message.metadata[$u].tools[] | {id, approved: ($d == "approve")} + (if $d == "approve" then {} else {rejection_reason: $r} end)]' <<<"$task")

rpc "$(jq -n --arg t "$task_id" --arg c "$SESSION_ID" --arg u "$URI" --argjson a "$approvals" '
  {jsonrpc:"2.0",id:"decide",method:"SendMessage",params:{message:{
    messageId:("decision-" + $t + "-" + (now|tostring)), role:"ROLE_USER", taskId:$t, contextId:$c,
    parts:[{text:"decision"}], extensions:[$u],
    metadata:{($u):{type:"tool_approval_response",approvals:$a}}}}}')" \
  | jq -r 'if .error then "error: \(.error.message)" else "sent (\(.result.task.status.state // .result.message.role // "ok"))" end'
