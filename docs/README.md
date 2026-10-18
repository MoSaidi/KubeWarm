# KubeWarm documentation

Start with the [root README](../README.md) for the project overview and feature summary. These guides contain installation details and the full technical reference.

| Guide | Contents |
| --- | --- |
| [Installation and CI](installation.md) | Build, kind runbook, CI warming requests and verification. |
| [Architecture](architecture.md) | Components, state, image lifecycle and operational boundaries. |
| [Preheating](preheat.md) | Webhook, seed selection, registry access and ranking metrics. |
| [Layer distribution](propagation.md) | Blob spreading, fallbacks, limits and progress. |
| [Image pull recovery](rescue.md) | Detection and recovery of blocked image pulls. |
| [Transfers](transfers.md) | Peer transfer protocol, blob/snapshot decisions and integrity. |
| [Registry mirror](mirror.md) | containerd mirror behavior, requirements and metrics. |
| [Image cleanup](image-cleanup.md) | Cleanup rules, rollback retention and metrics. |
| [Node settings](node-settings.md) | containerd and kubelet settings and limits. |
| [Configuration](configuration.md) | Environment variables, defaults and Helm settings. |
| [HTTP API](api.md) | Controller and worker endpoints. |
| [Observability](observability.md) | Prometheus metrics, Grafana dashboard and origin checks. |
| [Operations](operations.md) | Status, logs, troubleshooting, upgrades and uninstall. |
| [Configuration recipes](recipes.md) | Suggested settings for common cluster needs. |
| [Security](security.md) | Privileges, trust boundaries, credentials and hardening. |
| [Development and validation](development.md) | Source layout, test coverage and validation results. |
