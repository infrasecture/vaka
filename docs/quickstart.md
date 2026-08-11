# First Secure Codex Session

This guide installs the published Codex recipe, starts it through Vaka, and
leaves you attached to a usable Codex session.

## Before You Start

You need Docker with Compose v2 and the Vaka CLI. Follow the
[installation guide](installation.md), then check the Docker target and prepare
the Vaka runtime:

```bash
vaka doctor --fix
```

The Codex launcher requires Bash 4.4 or newer. Linux distributions normally
provide it. On macOS, install a current Bash with Homebrew if the launcher asks
for one.

## Install The Recipe

Install the latest published Codex recipe into a new directory:

```bash
mkdir -p "$HOME/vaka-recipes"
vaka get codex "$HOME/vaka-recipes/codex"
```

`vaka get` verifies and validates the downloaded recipe. It installs files only;
it does not start containers or execute the recipe.

The install summary currently reports `codex:unpinned-image`. This is expected:
the cross-platform Codex workstation uses a fixed version tag, while the
LiteLLM image is pinned by digest.

## Start Codex

For a quick first session, launch from the recipe directory:

```bash
cd "$HOME/vaka-recipes/codex"
./myCodex
```

The launcher asks for a workspace name. Press Enter to use `work`; the project
files for this session will live under `.workspaces/work`. The recipe directory
itself, including its stored credentials, is never mounted into Codex.

Next, choose how the LiteLLM gateway should authenticate upstream:

1. ChatGPT subscription — complete the displayed browser device-login flow.
2. OpenAI API key — enter a key or provide `OPENAI_API_KEY` or
   `OPENAI_API_KEY_FILE`.
3. Google Vertex AI — an experimental profile that requires your own Vertex
   configuration.

`myCodex` handles that authentication flow, pulls images as needed, starts the
Codex and LiteLLM containers through Vaka, waits for the session to become ready,
and attaches your terminal. You are now using interactive Codex with the chosen
workspace mounted; model calls go through LiteLLM, while other direct outbound
connections are blocked.

## Use An Existing Project

Run the same launcher from the project you want Codex to edit:

```bash
cd /path/to/your/project
"$HOME/vaka-recipes/codex/myCodex"
```

The current directory is mounted read/write at the same absolute path inside
the Codex container. Files Codex creates remain owned by your host user. No
other project directory is shared unless you explicitly add another mount.

## Return Or Stop

Run the launcher again from the same project directory to reattach. Bring the
complete stack down when you no longer need it:

```bash
"$HOME/vaka-recipes/codex/myCodex"       # start if needed and attach
"$HOME/vaka-recipes/codex/myCodex" down  # remove the complete stack
```

To leave Codex running and return to your host shell, press `Ctrl-b`, then `d`.
Run the launcher from the same project directory when you want to reattach.

`down` preserves the project's Codex configuration and history volume. Do not
add `-v` unless you intentionally want to delete that state.

## Next

- [Codex Recipe: A Restricted Agent Workspace](examples.md) explains the
  containers, credentials, network boundary, common usage patterns, and recipe
  updates.
- [Write Your First `vaka.yaml`](vaka-yaml-quickstart.md) is the separate path
  for protecting services in an existing Compose project.
