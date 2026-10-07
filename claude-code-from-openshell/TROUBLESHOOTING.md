# Troubleshooting

Problems you may hit while running Claude Code on kagent 1.x and Agent Substrate, and how to check them. The commands assume the `kagent` namespace and the `kagent-default` worker pool from the [README](README.md). Items marked *(source)* come from reading the kagent `v1.0.0-alpha7` code and were not triggered during testing.

## The agent is not `READY`

```bash
kubectl get agent claude -n kagent -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
```

- `ActorTemplate golden snapshot` pending: normal for the first minute or two after you create or change the Harness. Wait for it.
- `ResolvedRefs=False`: a name in `claude.yaml` does not match. Check that `anthropic-model-config`, `claude-template`, and `claude-harness` exist in `kagent`.
- The image reference is a tag, not a digest. Substrate rejects a tag alone, because a revision must be reproducible. Use the `@sha256:` form from `docker buildx imagetools inspect ghcr.io/kagent-dev/kagent/claude-harness:1.0.0-alpha7`. The tag has no `v` in front (`v1.0.0-alpha7` does not exist). *(source)*

## The template or Harness is rejected at compile time

*(source)* The Claude runtime is stricter than the kagent engine. The message appears in the Agent conditions:

- `Claude does not support Anthropic provider options beyond baseUrl yet`: remove `promptCaching` and anything else under `anthropic:` except `baseUrl`. Use a separate `ModelConfig` for agents that want prompt caching.
- `Claude does not support ModelConfig defaultHeaders, TLS, or apiKeyPassthrough yet`: remove those fields from the `ModelConfig`.
- `Harness env "..." conflicts with Claude-owned runtime configuration`: you set a variable the harness owns, such as `ANTHROPIC_API_KEY`, `CLAUDE_CONFIG_DIR`, a CA variable, or an `OTEL_*` capture variable. Remove it. Credentials go on the `ModelConfig` or `RemoteMCPServer`, never in `env`, which accepts literal values only.

## `input was not accepted; retry after the session becomes available`

The CLI prints this and the session stays `RUNNING`. I did not hit it in this example, but I did with the Pi example on the same cluster, and the checks are the same. Start with capacity:

```bash
kubectl get workerpools -n kagent
kubectl ate get actors --atespace kagent
```

Sessions stuck in `RUNNING` never suspend and keep using capacity. Delete stale sessions with `kagent agent session delete <id>`. See [item 1 and 3 of the Pi troubleshooting guide](../pi-agent/TROUBLESHOOTING.md) for scaling the pool through Helm and for a worker pod the router cannot reach.

## The CLI says `Claude requires approval before calling ...` and stops

This is not an error. `requireApproval: true` paused the task and it is waiting for you. The actor is `ACTOR_STATE_PAUSED` and holds no worker. Look at what is pending, then answer it in the kagent UI or with the script:

```bash
./scripts/approve.sh $SESSION_ID pending
./scripts/approve.sh $SESSION_ID          # approve; or: reject "reason"
```

`no pending approval in session ...` means the task is not waiting: it already finished, or it is still working. Check the state with `kubectl ate get actors --atespace kagent`.

The script needs the controller port-forward (`kubectl port-forward -n kagent svc/kagent-controller 8083:8083`). A `curl: (7)` error means the port-forward is not running.

## The GitHub MCP server is not accepted, or it lists no tools

```bash
kubectl get remotemcpserver github-mcp -n kagent -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
```

- You should see `Accepted=True Discovered 4 MCP tools`.
- The Secret must hold the full header value, `Bearer <token>`, under the key you named in `headersFrom`. The gateway replaces the whole header, so a bare token without `Bearer ` should be refused by GitHub *(not tested)*.
- The token needs access to the repository you name in the prompt. A fine-grained token only sees the repositories you selected.
- If Claude can call the tools but a push fails after you approve it, read the tool result in the session. the tool result is where GitHub's reason appears *(not tested: every push in my runs succeeded after approval)*.

## `actor egress policy denied` in the gateway logs

Watch for denials with:

```bash
kubectl logs -n ate-system deploy/atenet-egress -c agentgateway -f | grep 'egress policy denied'
```

Each line names the actor and the host. Two denials are expected:

- `github.com`, when Claude runs `git clone` or `git push`. By design: GitHub is reachable only through the MCP server in `github-push.yaml`.
- `http-intake.logs.us5.datadoghq.com`, from a stock session. Set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` in the Harness `env`, as `claude.yaml` does.

Any other host means something in your prompt or tools reached for a destination the template does not name. To allow it, name it in the template as a model endpoint, an MCP server, an HTTP tool, or a skill source. There is no field for an arbitrary host.

## A new session does not pick up my change

A Harness or template change produces a new revision, and a new session uses it. I confirmed that for new sessions. I did not test what an already-open session does, so delete it and create a new one when you change `claude.yaml` or `github-push.yaml`.
