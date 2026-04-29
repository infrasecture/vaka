# Examples

Examples live under [`examples/`](../examples). They are intended to be copied, adapted, and used as policy starting points for real Compose stacks.

## Codex With LiteLLM Gateway

Path: [`examples/codex`](../examples/codex)

This example runs Codex in one container and LiteLLM in a separate local gateway container.

- `codex` can resolve DNS and connect only to `litellm:4000`.
- `litellm` has the internet-facing model-provider allowlist.
- The real provider API key stays in the gateway, not in the Codex container.

Run it from the project directory you want Codex to work on:

```bash
/path/to/vaka/examples/codex/myCodex
```

Common commands:

```bash
/path/to/vaka/examples/codex/myCodex ps
/path/to/vaka/examples/codex/myCodex stop
/path/to/vaka/examples/codex/myCodex exec bash
```

See [`examples/codex/README.md`](../examples/codex/README.md) for the full walkthrough.

## Recommended Agent Pattern

For agent containers, prefer a sidecar or gateway pattern:

- The agent container is blocked by default.
- The agent can reach only local services it needs.
- Internet-facing access lives in a narrower gateway service.
- Each gateway has its own explicit egress allowlist.

This is usually safer than allowing the agent container to reach model providers, package registries, GitHub, arbitrary docs sites, and internal systems directly.

## Adapting Existing Compose Agent Stacks

vaka can usually be added to an existing Compose stack without changing the Compose file:

1. Identify the service that runs the agent loop.
2. List the external endpoints it actually needs.
3. Add DNS plus those endpoints to `vaka.yaml`.
4. Run `vaka validate --compose docker-compose.yaml`.
5. Start with `vaka up` instead of `docker compose up`.

Common candidates include self-hosted coding agents, browser/tool sandboxes, model gateways, package-cache sidecars, and MCP gateway services.

Examples of stacks where the same pattern can apply:

- [OpenHands](https://github.com/OpenHands/OpenHands)
- [OpenClaw](https://github.com/openclaw/openclaw)
- [SwarmClaw](https://github.com/swarmclawai/swarmclaw)
- [Docker Compose for Agents](https://github.com/docker/compose-for-agents)

Treat the links as integration targets, not as tested official vaka examples.

## Other Useful Patterns

The same policy model applies outside coding agents:

- Vendor or SaaS connector containers that should call only the vendor's published endpoints.
- CI and build containers that should reach package registries and artifact stores, not production services.
- Dev and staging services that should not accidentally connect to production systems.
- Data-processing jobs that should egress only to approved warehouses, logs, or object stores.
- Suspicious binary analysis where the process should have no network access or only a narrow allowlist.
- Plugin or extension containers that need their own explicit egress contract.
