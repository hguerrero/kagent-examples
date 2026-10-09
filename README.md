# kagent examples

Worked examples for [kagent](https://kagent.dev) 1.x and Agent Substrate. Each example is self-contained, was run end to end on a real cluster, and comes with the manifests, scripts, and commands to run it.

## Examples

| Example | What it shows | Tested with |
| :--- | :--- | :--- |
| [`claude-code-from-openshell`](claude-code-from-openshell/) | Migrate Claude Code from the NVIDIA OpenShell GitHub push tutorial to Agent Substrate with kagent's built-in Claude runtime (no adapter), gateway credential injection, a GitHub MCP server with approval gates, and suspend and resume | kagent 1.0.0-alpha7, Substrate 0.3.0-alpha3 |
| [`pi-agent`](pi-agent/) | Run the Pi coding agent on Agent Substrate through kagent's bring-your-own runtime, with an A2A adapter, OpenRouter credential injection, and suspend and resume that keeps the conversation | kagent 1.0.0-alpha7, Substrate 0.3.0-alpha3 |
| [`openclaw-agent`](openclaw-agent/) | Run OpenClaw on Agent Substrate through kagent's bring-your-own runtime, with an A2A-to-ACP adapter, exec approvals that pause the actor, and an agentgateway that limits the harness to four read-only Kubernetes tools. The kagent 1.x version of the 0.10.1 `AgentHarness` setup | kagent 1.0.0-alpha7, Substrate 0.3.0-alpha3 |
| [`agentgateway-trusted-proxy`](agentgateway-trusted-proxy/) | Run kagent with real user identity: OIDC sign-in through Dex, the bundled oauth2-proxy, the controller in `trusted-proxy` mode, a gateway for TLS and routing, and the NetworkPolicies the chart does not ship. Lists what it gives you and what it does not, including the missing authorization. Needs only a running kagent 1.x | kagent 1.0.0-alpha7, oauth2-proxy 7.15.5, Dex v2.46.0, agentgateway v1.6.0 |

## Prerequisites

The examples that run agents on Agent Substrate (`claude-code-from-openshell`, `pi-agent`, `openclaw-agent`) need the same base setup. `agentgateway-trusted-proxy` needs only a running kagent 1.x and lists its own prerequisites.

The Substrate examples need:

- A Kubernetes cluster (1.37 or later, with the `certificates.k8s.io/v1beta1` API enabled) running Agent Substrate and kagent 1.x, with a worker pool named `kagent-default`.
- On your machine: `kubectl`, `helm`, the `kagent` CLI, the `kubectl-ate` plugin, `jq`, and `openssl`. Docker is needed for examples that build an image.

An example's own README lists anything it needs beyond this, such as API keys.

### Install Agent Substrate and kagent

The [kagent installation guide](https://kagent.dev/docs/kagent/1.x/setup/installation/) is the source of truth. The commands below show the versions these examples were tested with.

1. Install the Substrate CRDs and runtime into the `ate-system` namespace:

   ```bash
   helm upgrade --install substrate-crds \
     oci://ghcr.io/kagent-dev/substrate/helm/substrate-crds \
     --version 0.3.0-alpha3 \
     --namespace ate-system --create-namespace

   helm upgrade --install substrate \
     oci://ghcr.io/kagent-dev/substrate/helm/substrate \
     --version 0.3.0-alpha3 \
     --namespace ate-system \
     -f - <<EOF
   credentialProvider:
     namespacePolicies:
     - atespace: kagent
       allowedNamespaces: [kagent]
   EOF
   ```

2. Create the identity material (the CA and JWT pools) with the `kubectl ate admin` commands, extract the root certificate, and roll Substrate out again with `--reuse-values`. Follow the installation guide for this step. It is not reproduced here, and it is where most first-run problems come from.

3. Every example here relies on credential injection, which needs one more pool: the gateway CA that lets the egress gateway add credentials to HTTPS requests. Create it next to the other pools, before you wait for the Substrate rollout:

   ```bash
   kubectl ate admin make-ca-pool --ca-id=1 --name=egress-mitm-ca-pool \
     --secret-namespace=ate-system --key-type=ECDSAP256
   ```

4. Install kagent with the Substrate integration on and a worker pool created for you. Each example creates its own `Secret` and `ModelConfig`, so the default provider here is your choice. This uses OpenAI, as in the installation guide:

   ```bash
   export OPENAI_API_KEY="your-api-key-here"

   helm upgrade --install kagent-crds \
     oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
     --version 1.0.0-alpha7 \
     --namespace kagent --create-namespace --wait

   helm upgrade --install kagent \
     oci://ghcr.io/kagent-dev/kagent/helm/kagent \
     --version 1.0.0-alpha7 \
     --namespace kagent --create-namespace --timeout 10m \
     -f - <<EOF
   providers:
     default: openAI
     openAI:
       apiKey: ${OPENAI_API_KEY}
   controller:
     grpc:
       reflection: true
     substrate:
       enabled: true
       ateApiEndpoint: dns:///api.ate-system.svc:443
       atenetRouterURL: http://atenet-router.ate-system.svc:80
   substrateWorkerPool:
     create: true
     replicas: 3
     workerImage: "ghcr.io/kagent-dev/substrate/ateom-gvisor:v0.3.0-alpha3"
   EOF
   ```

5. Verify, and install the `kagent` CLI at the same version as the controller:

   ```bash
   kubectl rollout status deployment/kagent-controller -n kagent --timeout=300s
   kubectl get workerpools -n kagent    # expect kagent-default

   curl https://raw.githubusercontent.com/kagent-dev/kagent/refs/heads/main/scripts/get-kagent | bash -s -- --version v1.0.0-alpha7
   ```

Change the pool size later through Helm (`helm upgrade kagent ... --reuse-values --set substrateWorkerPool.replicas=<n>`), not with `kubectl scale`: Helm owns the replica count, and a later `helm upgrade` fails with a field conflict if you changed it behind its back.

The test clusters were installed once with these versions. The identity-material step and a from-scratch install were not re-run for each example.

## How the examples are organized

- Each example lives in its own folder with its own `README.md`, and you can run it without the rest of the repo.
- Each README has a "Tested with" table. kagent 1.x and Substrate are alpha releases that change often, so versions are pinned per example and not for the whole repo. Check the [kagent 1.x docs](https://kagent.dev/docs/kagent/1.x/) for current ones.
- Container images are referenced by digest, because Substrate pins images that way. Build and push your own to avoid depending on a prebuilt one.
- A `TROUBLESHOOTING.md` next to an example's README covers the errors most likely to hit.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
