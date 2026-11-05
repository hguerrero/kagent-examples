# kagent examples

Worked examples for [kagent](https://kagent.dev) 1.x and Agent Substrate. Each example is self-contained, was run end to end on a real cluster, and comes with the manifests, scripts, and commands to run it.

## Examples

| Example | What it shows | Tested with |
| :--- | :--- | :--- |
| [`claude-code-from-openshell`](claude-code-from-openshell/) | Migrate Claude Code from the NVIDIA OpenShell GitHub push tutorial to Agent Substrate with kagent's built-in Claude runtime (no adapter), gateway credential injection, a GitHub MCP server with approval gates, and suspend and resume | kagent 1.0.0-rc2, Substrate 1.0.0-rc2 |
| [`pi-agent`](pi-agent/) | Run the Pi coding agent on Agent Substrate through kagent's bring-your-own runtime, with an A2A adapter, OpenRouter credential injection, and suspend and resume that keeps the conversation | kagent 1.0.0-rc2, Substrate 1.0.0-rc2 |
| [`openclaw-agent`](openclaw-agent/) | Run OpenClaw on Agent Substrate through kagent's bring-your-own runtime, with an A2A-to-ACP adapter, exec approvals that pause the actor, and an agentgateway that limits the harness to four read-only Kubernetes tools. The kagent 1.x version of the 0.10.1 `AgentHarness` setup | kagent 1.0.0-rc2, Substrate 1.0.0-rc2 |
| [`agentgateway-trusted-proxy`](agentgateway-trusted-proxy/) | Run kagent with real user identity: OIDC sign-in through Dex, the bundled oauth2-proxy, the controller in `trusted-proxy` mode, a gateway for TLS and routing, and the NetworkPolicies the chart does not ship. Records what works and what does not, including the missing authorization. Needs only a running kagent 1.x | kagent 1.0.0-rc2, oauth2-proxy 7.15.5, Dex v2.46.0, agentgateway v1.6.0 |

> **rc2 status.** All four examples run on kagent 1.0.0-rc2. `pi-agent` and `openclaw-agent` run your own adapter image, so you build and push one first, and each README has the commands. Their manifests carry the maintainer's `hguerrero0/pi-agent:1.0.0-rc2` and `hguerrero0/openclaw-agent:1.0.0-rc2` digests as an example of the reference format and for the maintainer's tests of the instructions. Do not depend on them.

## Prerequisites

The examples that run agents on Agent Substrate (`claude-code-from-openshell`, `pi-agent`, `openclaw-agent`) need the same base setup. `agentgateway-trusted-proxy` needs only a running kagent 1.x and lists its own prerequisites.

The Substrate examples need:

- A Kubernetes cluster, 1.37 or later, that serves the `PodCertificateRequest` and `ClusterTrustBundle` APIs (a default `kind` node on v1.37 does), running Agent Substrate and kagent 1.x, with a worker pool named `kagent-default`.
- On your machine: `kubectl`, `helm`, the `kagent` CLI, the `kubectl-ate` plugin, and `jq`. Docker and a registry you can push to are needed for the examples that run an adapter image (`pi-agent` and `openclaw-agent`).

An example's own README lists anything it needs beyond this, such as API keys.

### Install Agent Substrate and kagent

The [kagent installation guide](https://kagent.dev/docs/kagent/1.x/setup/installation/) is the source of truth. The `kagent` CLI installs the CRDs, Substrate with its identity material and the egress gateway CA, a development PostgreSQL, kagent, and the `kagent-default` worker pool:

```bash
curl https://raw.githubusercontent.com/kagent-dev/kagent/refs/heads/main/scripts/get-kagent | bash -s -- --version v1.0.0-rc2

export OPENAI_API_KEY=unused   # the installer requires a default provider key; each example creates its own Secret and ModelConfig
kagent install --namespace kagent
kubectl rollout status deployment/kagent-controller -n kagent --timeout=300s
kubectl get workerpools -n kagent    # expect kagent-default
```

Wait for the node to be `Ready` before you run `kagent install`. Each Helm step waits five minutes, and a pod stuck on the control-plane taint can use that up.

Then install the Substrate CLI plugin. Substrate `v1.0.0-rc2` has no release binary, so use the `v1.0.0-rc1` one that the kagent docs pin:

```bash
curl -fsSL -o kubectl-ate \
  "https://github.com/kagent-dev/substrate/releases/download/v1.0.0-rc1/kubectl-ate-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
chmod +x kubectl-ate && sudo mv kubectl-ate /usr/local/bin/
```

`kagent install` takes no Helm flags. To change a chart value later, such as the pool size, put it in a values file and point a CLI config at it. Use this instead of `kubectl scale`, which a later install would fight with a field conflict. Run it with the same provider key as before, because each run sets its own values and keeps none from the last one:

```bash
cat > kagent-values.yaml <<EOF
substrateWorkerPool:
  replicas: 3
EOF
cat > kagent-config.yaml <<EOF
install:
  kagent:
    valuesFiles: [kagent-values.yaml]
EOF
export OPENAI_API_KEY=<the provider key you installed kagent with>
kagent --config kagent-config.yaml install --namespace kagent
```

On 2026-11-05 `kagent install` worked in one pass on fresh `kind` clusters, and every example was tested on a cluster installed this way. Plan for about 4 GB of memory for the cluster: with 2.6 GB the controller stayed `Pending`. See the example READMEs for what each one ran.

## How the examples are organized

- Each example lives in its own folder with its own `README.md`, and you can run it without the rest of the repo.
- Each README has a "Tested with" table. kagent 1.x and Substrate are release candidates that can still change, so versions are pinned per example and not for the whole repo. Check the [kagent 1.x docs](https://kagent.dev/docs/kagent/1.x/) for current ones.
- Container images are referenced by digest, because Substrate pins images that way. For the examples with an adapter, you build and push your own image and put its digest in the manifest. The digests already in the manifests are the maintainer's test images, kept as an example of the format.
- A `TROUBLESHOOTING.md` next to an example's README covers the errors most likely to hit.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
