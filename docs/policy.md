# Policy Reference

`vaka.yaml` is a service policy file. It is separate from `docker-compose.yaml`; service names in both files must match.

## Minimal Shape

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  <service-name>:
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

## Full Schema

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  <service-name>:
    network:
      egress:
        defaultAction: reject
        with_tcp_reset: true
        block_metadata: drop
        accept: [<rule>, ...]
        reject: [<rule>, ...]
        drop: [<rule>, ...]
    runtime:
      dropCaps: [NET_RAW, SYS_ADMIN]
      chown:
        - path: /data
        - path: /var/cache/app
          owner: "1000:1000"
          recursive: true
```

Host-authored policy files must not set `vakaVersion` or `services.<name>.user`. Those fields are generated internally for the injected per-service policy consumed by `vaka-init`.

## Rule Evaluation Order

Generated nftables rules are evaluated in this order:

1. Established and related traffic.
2. Loopback.
3. Metadata endpoint rule from `block_metadata`, when configured.
4. `drop` rules.
5. `reject` rules.
6. `accept` rules.
7. `defaultAction`.

This order is fixed. The `accept`, `reject`, and `drop` lists are not an arbitrary ordered firewall program.

## DNS

Allow DNS using the container's resolver:

```yaml
accept:
  - dns: {}
```

Specify DNS servers explicitly:

```yaml
accept:
  - dns:
      servers: [1.1.1.1, 8.8.8.8]
```

The shorthand allows UDP and TCP port 53 to the resolver addresses.

## Address And Port Rules

```yaml
accept:
  - proto: tcp
    to:
      - api.example.com
      - 10.0.0.0/8
      - 192.168.1.10
    ports:
      - 443
      - "8080-8090"
```

Valid protocols are `tcp`, `udp`, `icmp`, and `icmpv6`.

`proto` is required when `ports` are set. `to` entries may be hostnames, literal IPs, or CIDRs. Hostnames are resolved inside the container at startup.

## Protocol-Only Rules

```yaml
drop:
  - proto: icmp
```

This matches all traffic for that protocol.

## ICMP Types

```yaml
accept:
  - proto: icmp
    type: echo-request
```

Numeric types are also accepted:

```yaml
accept:
  - proto: icmp
    type: 8
```

Known ICMP names include `echo-request`, `echo-reply`, `destination-unreachable`, `time-exceeded`, `redirect`, `parameter-problem`, `timestamp-request`, and `timestamp-reply`.

Known ICMPv6 names include `nd-neighbor-solicit`, `nd-neighbor-advert`, `nd-router-solicit`, `nd-router-advert`, `mld-listener-query`, and `mld-listener-report`.

## `defaultAction`

| Value | Behavior |
|-------|----------|
| `reject` | Default. Unmatched TCP receives TCP RST; other protocols receive ICMP `admin-prohibited`. |
| `drop` | Unmatched packets are silently discarded. |
| `accept` | Unmatched packets are allowed. Use only for blocklist-style policies. |

`defaultAction: accept` is useful for transitional blocklists but weakens the allowlist model.

## `with_tcp_reset`

When `defaultAction: reject`, `with_tcp_reset` controls whether unmatched TCP receives a TCP RST.

```yaml
defaultAction: reject
with_tcp_reset: false
```

The same option is valid on `reject` rules with `proto: tcp`:

```yaml
reject:
  - proto: tcp
    to: [10.0.0.1]
    ports: [22]
    with_tcp_reset: false
```

It is invalid on `accept` rules, `drop` rules, non-TCP reject rules, or non-reject defaults.

## `block_metadata`

`block_metadata` adds rules for common cloud instance metadata endpoints:

```yaml
block_metadata: drop
```

Mapping form:

```yaml
block_metadata:
  action: reject
  with_tcp_reset: false
```

Covered endpoints:

| Address | Provider |
|---------|----------|
| `169.254.169.254/32` | AWS, GCP, Azure, DigitalOcean, Hetzner, OCI, Linode |
| `100.100.100.200/32` | Alibaba Cloud |
| `fd00:ec2::254/128` | AWS IPv6 IMDS |
| `fd20:ce::254/128` | GCP IPv6 IMDS |

## `runtime.dropCaps`

vaka adds `NET_ADMIN` so `vaka-init` can load nftables rules. By default, `vaka-init` drops the capabilities vaka added before it starts your app. If the service already had `NET_ADMIN` in `cap_add`, vaka treats that as intentional and does not remove it automatically.

Set `runtime.dropCaps` only when you want to control the complete drop list yourself:

```yaml
runtime:
  dropCaps: [NET_ADMIN, NET_RAW, SYS_PTRACE]
```

Both `NET_ADMIN` and `CAP_NET_ADMIN` forms are accepted.

## `runtime.chown`

Use `runtime.chown` to fix ownership of mounted paths before the app starts:

```yaml
runtime:
  chown:
    - path: /data
    - path: /var/cache/app
      owner: "app:app"
      recursive: true
```

Rules:

- `path` is required and must be absolute.
- `owner` uses `user[:group]`, `uid`, or `uid:gid` syntax.
- If `owner` is omitted, vaka uses the generated service user.
- `recursive` defaults to `false`.
- The path must exist, be on a writable mount, and not resolve to the root filesystem mount.

Any violation fails closed before the application starts.

## Limits Of The Current DSL

The `egress` DSL is intentionally simple. It is good for common allowlist and blocklist policies, but it is not a full nftables language.

Current limits:

- You cannot arbitrarily interleave accept, reject, and drop rules.
- `block_metadata` has fixed precedence ahead of user rules.
- Native `.nft` and nft JSON pass-through are not implemented yet.

Track the native nft escape-hatch work in [issue #19](https://github.com/infrasecture/vaka/issues/19).
