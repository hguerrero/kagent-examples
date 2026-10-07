# Troubleshooting

Problems you may hit while running Pi on kagent 1.x and Agent Substrate, and how to check them. The commands assume the `kagent` namespace and the `kagent-default` worker pool from the [README](README.md).

## `input was not accepted; retry after the session becomes available`

The CLI prints this and the session stays `RUNNING`. The gateway refused to hand your message to the actor, or the actor failed before it saved its first event. Check these in order:

1. **Capacity.** Run `kubectl get workerpools -n kagent`. Every running actor holds a worker, and sessions stuck in `RUNNING` never suspend, so they keep holding it. Delete stale sessions with `kagent agent session delete <id>`, and scale the pool up if needed. Scale through Helm so the release and the cluster agree (a `kubectl scale` makes later `helm upgrade` runs fail with a field conflict):

   ```bash
   helm upgrade kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
     --version 1.0.0-alpha7 --namespace kagent --reuse-values \
     --set substrateWorkerPool.replicas=3
   ```

2. **The adapter's first event.** kagent's ADK wrapper emits the task's first event itself. An adapter that also emits a submitted task produces a conflicting write, and the dispatch is revoked. The adapter in `adapter/main.go` does not emit one, so this only applies if you change it.

3. **A worker the router cannot reach.** List the actors:

   ```bash
   kubectl ate get actors --atespace kagent
   ```

   If a session's actor sits in `ACTOR_STATE_RUNNING` on one worker pod while turns keep failing, check the router for `Connect: deadline has elapsed`:

   ```bash
   kubectl logs -n ate-system deploy/atenet-router -c agentgateway | grep 'deadline has elapsed'
   ```

   In the test cluster, the one worker pod whose container had restarted could not be reached, while the two fresh workers were fine. Every restore onto that worker failed, including a new session's first turn. Deleting the pod let the pool replace it, and every turn after that worked:

   ```bash
   kubectl delete pod -n kagent <worker-pod>
   ```

   The link to the container restart is a correlation, not a confirmed root cause. Deleting a worker pod stops any actor running on it.

4. **Logs.** Look for non-OK `rpc completed` lines in the controller:

   ```bash
   kubectl logs -n kagent deploy/kagent-controller | grep -v '"grpc_code":"OK"'
   ```

   Then check the actor and worker pods. If you are stuck, `kagent bug-report -n kagent` collects what the maintainers will ask for. The report holds cluster logs, so review it before you share it or commit it.

## The agent forgets earlier turns, but files are still there

The durable volume keeps `/data`, but a resume starts a new Pi process. Make sure you are running the adapter from this repo, which restores the Pi session, and that `/data/adapter/pi-session` exists after the first turn. On resume, the actor log should show `restored the previous pi session`:

```bash
kubectl ate logs actors session-<session-id> -a kagent
```

The actor must be running to read its log. If it has already suspended, send another message first.

## `actor egress policy denied` in the gateway logs

Watch for denials with:

```bash
kubectl logs -n ate-system deploy/atenet-egress -c agentgateway -f
```

A request to `pi.dev` is Pi fetching its model catalog at startup. The default-deny policy blocks it with a 403, and Pi carries on without it. `PI_OFFLINE=1` in `pi.yaml` stops the call. A denial for any other host means the destination is not named in the `AgentTemplate`, so the agent cannot reach it.
