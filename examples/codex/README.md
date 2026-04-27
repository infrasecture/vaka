# Secure Codex Container With Vaka

This example runs Codex inside a Docker container whose outbound network access is restricted by vaka. The goal is to let an agent work on your code while limiting what it can send to the internet.

The setup uses two containers:

- `codex`: the agent harness where Codex runs and where your project is mounted at `/workspace`.
- `litellm`: a local LLM gateway that receives Codex model requests and forwards them to the model provider.

Vaka blocks direct egress from the Codex container. Codex can talk to the LiteLLM sidecar, but it cannot directly call arbitrary websites, pastebin services, webhooks, package hosts, or other internet endpoints. LiteLLM is the only path from the agent container to the LLM provider.

## Why This Exists

LLM agents can be tricked by prompts, repository content, test output, or tool results. A malicious instruction might ask the agent to reveal API keys, upload private source code, or send sensitive files to an external server.

This example reduces that risk by isolating the agent inside a container with blocked egress. Even if the agent is jailbroken or prompted to exfiltrate data, the Codex container cannot directly connect to the internet. Its blast radius is the code and files mounted into `/workspace`.

This does not make unsafe code safe, and it does not hide files that you place inside the mounted workspace. If a secret is present under the project directory you run from, the agent may be able to read it. The protection is that the agent should not have a direct network path to send that secret somewhere else.

## What You Need

- Docker with Compose support.
- `vaka` installed and available on your `PATH`.
- An OpenAI API key, provided when prompted or through `OPENAI_API_KEY`.

If `vaka` is missing, `myCodex` will warn you before falling back to plain Docker Compose. Running without vaka removes the egress protection.

## How To Run It

Run `myCodex` from the project directory you want the agent to work on, not from this example directory.

```sh
cd /path/to/your/project
/path/to/examples/codex/myCodex
```

On first run, `myCodex` asks for your `OPENAI_API_KEY` if it is not already set. It then starts the container stack and attaches you to the Codex session.

Your current project directory is mounted inside the Codex container as:

```text
/workspace
```

That is the main boundary to keep in mind. Files inside the project directory are available to the agent. Files outside that directory are not mounted into the Codex container by this example.

Common commands:

```sh
/path/to/examples/codex/myCodex
/path/to/examples/codex/myCodex ps
/path/to/examples/codex/myCodex stop
/path/to/examples/codex/myCodex restart
/path/to/examples/codex/myCodex exec bash
```

## How The Network Boundary Works

The Codex container has a strict vaka egress policy:

- It can resolve DNS.
- It can connect to the `litellm` sidecar on port `4000`.
- Other outbound connections are rejected.

The LiteLLM sidecar has its own narrower allowlist for LLM-provider traffic. In this example it can reach the model provider endpoints needed to serve as the gateway.

In normal use, Codex sends model requests to:

```text
http://litellm:4000/v1
```

LiteLLM forwards those requests using the real provider API key. Codex receives only the temporary proxy key generated for the local LiteLLM service, not the real provider key.

## Practical Security Model

This example is meant to limit accidental or prompt-driven data exfiltration from the agent harness.

It helps with:

- A prompt injection that tells the agent to upload files to an external URL.
- A malicious repository instruction that asks the agent to leak environment variables.
- A compromised tool result that tries to make the agent call out to an attacker-controlled service.

It does not solve:

- Secrets that are already committed or stored inside the mounted project directory.
- Commands that change or delete files inside `/workspace`.
- Trust decisions about code the agent writes for you.
- All possible Docker, host, or kernel escape risks.

Treat `/workspace` as the allowed blast radius. Put only the project files the agent needs there, and keep unrelated secrets outside that directory.

## Extending The Setup

You can add more sidecar containers when the agent needs controlled access to a service. For example, instead of letting Codex reach the internet directly, add a local sidecar for the capability you want and allow Codex to talk only to that sidecar.

Possible extensions include:

- A package cache or internal registry proxy.
- A database container with test data.
- A documentation search service.
- A browser or web-fetch service with its own policy.
- Docker's MCP Gateway: https://docs.docker.com/ai/mcp-catalog-and-toolkit/mcp-gateway/

The pattern is the same: keep Codex blocked by default, give it access to a narrow local service, and put the internet-facing permissions on that service only when needed.

You can also relax `vaka.yaml` to allow Codex to reach specific hosts or services directly. That is possible, but it is not recommended as the default. Each extra allowed destination is another place a jailbroken or misled agent could send sensitive data.

## Stopping The Stack

From any project directory, run:

```sh
/path/to/examples/codex/myCodex stop
```

The next start recreates the stack with a fresh LiteLLM proxy key.
