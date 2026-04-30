# Troubleshooting

Start with:

```bash
vaka doctor
```

Then retry fixable checks:

```bash
vaka doctor --fix
```

## Helper Image Missing

`vaka doctor --fix` pulls `emsi/vaka-init:<vaka-version>`.

If you built a local development binary with version `dev`, there is no published `emsi/vaka-init:dev`. Build the helper image locally with `./build.sh` or use a stamped release binary.

## Version Mismatch After Upgrade

Existing containers may keep an older anonymous helper volume. Refresh it:

```bash
vaka down --volumes
vaka up
```

or renew anonymous volumes:

```bash
vaka up -V
```

## `network_mode: host`

vaka rejects services using `network_mode: host`. Those services share the host network namespace, so vaka cannot install a per-container egress policy.

Use a normal bridge network or move enforcement to a host/VM firewall layer.

## Build-Only Services

If a service uses `build:` with no `image:`, vaka may not be able to inspect the runtime entrypoint or user before build.

Fix by adding an image name:

```yaml
services:
  app:
    build: .
    image: app:local
```

or explicitly declare runtime metadata:

```yaml
services:
  app:
    build: .
    user: "1000:1000"
    entrypoint: ["/usr/local/bin/app"]
```

## DNS Or Hostname Surprises

Hostnames in policy are resolved inside the container when it starts. This is intentional: Docker embedded DNS, split-horizon networks, and CDN/anycast endpoints may resolve differently inside the container than on the host.

If an endpoint changes, restart the service so `vaka-init` resolves it again.

## Docker Context Or Remote Daemon

vaka follows the Docker CLI environment and active Docker context. Docker top-level flags such as `--context` and `--host` are not accepted as vaka arguments.

Use:

```bash
docker context use <name>
```

or environment variables such as:

```bash
DOCKER_CONTEXT=<name> vaka doctor
```

## Inspect Generated Output

Preview nftables rules:

```bash
vaka show-nft <service>
```

Preview the Compose override:

```bash
vaka show-compose
```

Write it to a file for inspection:

```bash
vaka show-compose -o /tmp/vaka-override.yaml
```

## Old Kernels

Very old Linux kernels may not support nftables features used by vaka. The failure should appear as an `nft` error before the app starts. See [issue #17](https://github.com/infrasecture/vaka/issues/17).
