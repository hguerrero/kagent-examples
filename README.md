# kagent examples

Worked examples for [kagent](https://kagent.dev) 1.x and Agent Substrate. Each example is self-contained, was run end to end on a real cluster, and comes with the files and commands a blog post or tutorial refers to.

## Examples

| Example | What it shows | Tested with | Post |
| :--- | :--- | :--- | :--- |
| [`pi-agent`](pi-agent/) | Run the Pi coding agent on Agent Substrate through kagent's bring-your-own runtime, with an A2A adapter, OpenRouter credential injection, and suspend and resume that keeps the conversation | kagent 1.0.0-alpha7, Substrate 0.3.0-alpha3 | [From OpenShell to Agent Substrate](TODO-POST-URL) |

## Prerequisites

Most examples need the same base setup:

- A Kubernetes cluster (1.37 or later) with Agent Substrate and kagent 1.x installed, including a worker pool named `kagent-default`. See the [kagent installation guide](https://kagent.dev/docs/kagent/1.x/setup/installation/).
- The tools on your machine: `kubectl`, the `kagent` CLI, the `kubectl-ate` plugin, and `jq`. Docker is needed for examples that build an image.

An example's own README lists anything it needs beyond this, such as API keys.

## How the examples are organized

- Each example lives in its own folder with its own `README.md`, and you can run it without the rest of the repo.
- Each README has a "Tested with" table. kagent 1.x and Substrate are alpha releases that change often, so versions are pinned per example and not for the whole repo. Check the [kagent 1.x docs](https://kagent.dev/docs/kagent/1.x/) for current ones.
- Container images are referenced by digest, because Substrate pins images that way. Build and push your own to avoid depending on a prebuilt one.
- A `TROUBLESHOOTING.md` next to an example's README covers the errors most likely to hit.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
