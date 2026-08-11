# Write Your First `vaka.yaml`

This guide adds outbound network policy to an existing Docker Compose project.
It is separate from installing a published recipe: you keep your Compose files
and write the policy for your own services.

Run the examples from the directory that contains your Compose project.

## 1. Match A Compose Service

Suppose your Compose file contains a service named `app`:

```yaml
services:
  app:
    image: your-application-image
```

The key under `services:` in `vaka.yaml` must use that exact service name. Only
services listed in `vaka.yaml` are managed by Vaka; other Compose services keep
their normal network access.

## 2. Add The Policy

Create `vaka.yaml` next to the Compose file:

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
        accept:
          - dns: {}
          - proto: tcp
            to: [api.example.com]
            ports: [443]
```

Replace `app` and `api.example.com` with your service and the destination it
actually needs. This policy means:

- reject outbound traffic that does not match an `accept` rule;
- silently drop access to common cloud metadata endpoints;
- allow DNS through the resolver configured inside the container; and
- allow TCP port 443 to the addresses resolved for `api.example.com` at startup.

Hostname rules are resolved when the container starts. Vaka filters IP addresses,
protocols, and ports; it does not inspect TLS SNI or HTTP hostnames. Keep the DNS
rule when a policy uses hostnames.

## 3. Validate It

Check the policy without contacting Docker or loading Compose:

```bash
vaka validate
```

That is the normal edit-time check. If you also want an explicit early check
that policy service names exist in a particular Compose model, supply its file:

```bash
vaka validate --compose compose.yaml
```

`--compose` is optional. Repeat it only when you need to validate an exact
multi-file Compose merge.

## 4. Start Through Vaka

Start the project with Vaka instead of Docker Compose:

```bash
vaka up -d
```

Vaka discovers the normal Compose project, checks the policy service names,
prepares its runtime, and creates the managed containers with a generated
override. Inside each managed container, `vaka-init` loads the nftables rules
before starting the application's original entrypoint. If policy installation
fails, the application does not start.

Running `docker compose up` directly bypasses Vaka's injection. Use `vaka up`,
`vaka run`, or the `vaka compose` namespace for container-creating commands.

## 5. Check The Result

Use the normal Vaka shorthands to operate the project:

```bash
vaka ps
vaka logs app
```

If the image contains `curl`, verify that the required endpoint still works
after replacing the example hostname:

```bash
vaka exec app curl -I https://api.example.com  # accepted by the policy
```

Connections to addresses outside the resolved allowlist are rejected. Two
different hostnames can share an allowed CDN address, so a hostname-to-hostname
`curl` comparison is not a reliable negative test of this IP-level boundary.

When you are finished with the project:

```bash
vaka down
```

## 6. Change And Reapply Policy

After editing `vaka.yaml`, validate and run `up` again:

```bash
vaka validate
vaka up -d
```

`up` applies the new policy revision and recreates a managed service when
needed. `vaka start` and `vaka compose restart` operate existing containers and
do not render a new policy, so they are not the right commands after a policy
change.

## Preview What Vaka Generates

These commands are useful once the basic flow works:

```bash
vaka show-nft app
vaka show-compose
```

`show-nft` prints the policy rules without applying them. Hostnames remain as
comments because their addresses are resolved inside the container at startup.
`show-compose` prints the generated Compose override without creating application
containers. Resolving the exact override may inspect, pull, or pre-build images.

For a nonstandard Compose filename, use the full namespace:

```bash
vaka compose -f compose.prod.yaml up -d
```

## Next

- [Policy Reference](policy.md) covers every rule type, metadata blocking,
  runtime capability handling, and mounted-path ownership.
- [How It Works](how-it-works.md) explains the generated Compose override and
  container startup sequence.
- [Troubleshooting](troubleshooting.md) covers Docker compatibility, build-only
  services, image inspection, and DNS behavior.
