# Troubleshooting

Problems you may hit while running OpenClaw on kagent 1.x and Agent Substrate, and how to check them. The commands assume the `kagent` namespace and the `kagent-default` worker pool from the [README](README.md).

An actor's log is readable only while the actor runs, and it suspends a few seconds after a turn ends. To read it, start the turn in the background and poll while it runs:

```bash
kagent agent invoke --session $SESSION_ID --task "..." &
until kubectl ate logs actors session-$SESSION_ID -a kagent > /tmp/actor.log 2>&1 \
  && ! grep -q "not currently running" /tmp/actor.log; do sleep 1; done
```

The log of an actor that resumed from its golden snapshot starts at the restore, so the adapter's startup lines are not in it.

## `input was not accepted; retry after the session becomes available`

The CLI prints this and the session stays `RUNNING`. The same checks apply as in the [`pi-agent` guide](../pi-agent/TROUBLESHOOTING.md#input-was-not-accepted-retry-after-the-session-becomes-available): capacity, a conflicting first event, a worker the router cannot reach, and the controller log. For OpenClaw, add one more cause.

**Every agent fails the same way, and the actor log says `connection reset by peer`.** The log of the failing actor shows this right after `Actor restored`:

```text
failed to save task state: rpc error: code = Unavailable desc = connection error: desc = "error reading server preface: connection reset by peer"
```

Check an agent that worked before, such as `pi`. If it fails too, the problem is in Substrate and not in this example. It was seen once, after the Docker VM ran short of memory during a multi-arch image build and `rustfs` (the snapshot store) was OOM-killed. Restarting the egress gateway and the router fixed it, and the existing agent worked again straight away. The root cause was not confirmed.

```bash
kubectl rollout restart deploy/atenet-egress deploy/atenet-router -n ate-system
kubectl rollout status deploy/atenet-egress -n ate-system
kubectl rollout status deploy/atenet-router -n ate-system
```

Delete the stuck sessions afterwards, because a session that failed this way keeps its worker: `kagent agent session delete <id>`.

## A turn fails with `OpenClaw returned no output.` or `OpenClaw could not complete the turn`

OpenClaw ends a turn with no text when its model call fails, and the reason is only in its own log. The adapter watches that log and, when it finds the error, puts it in the failure message. For example:

```text
OpenClaw could not complete the turn: anthropic/claude-sonnet-5-5 request failed (authentication failed, HTTP 401). Re-authenticate the provider and try again.
```

A 401 means the egress gateway did not inject the key. Check that the `ModelConfig` names a Secret that exists and has the key you set in `apiKeySecretKey`, and that the actor environment holds the placeholder:

```bash
kubectl get secret kagent-anthropic -n kagent -o jsonpath='{.data}' | jq 'keys'
kubectl get modelconfig anthropic-model-config -n kagent -o jsonpath='{.status.conditions}'
kubectl ate get actor-template $(kubectl ate get actor-template --atespace kagent | awk '/openclaw/ {print $2}' | tail -1) \
  --atespace kagent -o yaml | grep -A1 ANTHROPIC_API_KEY      # value: kagent-credential-injected
```

If the message is only `OpenClaw returned no output.`, read the actor log for the run and look for `embedded run agent end` lines with `isError=true`.

## `FsSafeError: path is not a regular file under root`

The adapter turns OpenClaw's native file helper off, because it fails under gVisor with this message for every file OpenClaw creates. If you see it, the setting was overridden. Check the `Harness` env in `openclaw.yaml` for `FS_SAFE_NATIVE_MODE`, and remove it or set it to `off`.

## The agent is `READY` but every invoke fails, and the log says the adapter stopped

`READY` on a `Harness` covers its dependencies only. The adapter checks the compiled configuration at start and stops with a message that names the problem. Read the actor log. These are the messages you can get:

| Message | Cause and fix |
| :--- | :--- |
| `the AgentTemplate needs a modelConfig` | The template has no `modelConfig`. OpenClaw has no default model. |
| `model type "..." is not supported` | The `ModelConfig` provider is not Anthropic or OpenAI. |
| `requireApproval on MCP server ... is not supported` | A tool binding sets `requireApproval`. OpenClaw approves `exec` commands, not MCP tool calls. Remove it and limit the tools with a gateway. |
| `stdio tools, remote agents and sub-agents are not supported` | The template binds something the adapter cannot map. |
| `OPENCLAW_EXEC_MODE="..." is not one of ...` | Use `full`, `auto`, `ask`, `allowlist`, or `deny`. |
| `header ... needs environment variable ..., which is not set` | An MCP header refers to a Secret value that did not reach the actor. |

## `Another Gateway owner lease is still active for this state directory`

OpenClaw keeps a lease on `/data/openclaw` and recovers it from a dead process by host name and start time. Inside a Substrate actor the host name is stable and this works. It can fail if you run the image somewhere else with a different host name each start: wait five minutes for the lease to expire, or start the container with a fixed `--hostname`.

## A task sits in `INPUT_REQUIRED` and `approve.sh` finds nothing

`approve.sh` looks up the session in the agent named by `AGENT`, which defaults to `openclaw`. If your agent has another name, set it: `AGENT=<name> ./scripts/approve.sh <session>`. It also needs the controller port-forward from the README.

A task is only pending when `OPENCLAW_EXEC_MODE` is `ask` or `auto`, and only for a command that is not on OpenClaw's allowlist. In `ask` mode OpenClaw also refuses some forms outright, such as a command with a redirection (`>`), without asking. Try a plain command like `hostname`.

## `actor egress policy denied` in the gateway logs

Watch for denials with:

```bash
kubectl logs -n ate-system deploy/atenet-egress -c agentgateway -f | grep -E "http.status=40[0-9]|reason=Authorization"
```

Requests to `clawhub.ai` are OpenClaw's skills registry. They are denied and harmless. A denial for any other host means the destination is not named in the `AgentTemplate`, so the agent cannot reach it. A tool server is allowed when a `RemoteMCPServer` in the template names it.

## The tool list is empty, or a tool is missing

Check what the gateway publishes, and what kagent discovered:

```bash
kubectl -n agentgateway-system get agentgatewaypolicy,agentgatewaybackend,httproute
kubectl get remotemcpserver sre-tools -n kagent -o jsonpath='{.status.discoveredTools[*].name}'
python3 scripts/mcp-list.py http://127.0.0.1:18091/mcp
```

A tool the policy does not match is missing from the list on purpose. OpenClaw names a tool after the server and the tool, in camel case, as in `k8sGetPodLogs`. A new session picks up a template change. An existing session keeps the revision it started with.

## OpenClaw is slow, or reports `memory pressure: critical`

The gateway needs about 1 GB. Harness actors in this setup have no memory limit, but a standalone sandbox from `kagent sandbox create` gets 1 GiB by default and OpenClaw stalls in it. The adapter logs `actor resources` with the memory and CPUs it sees when it starts OpenClaw. Raise `controller.sandbox.memory` in the kagent Helm values if you need a sandbox for debugging.
