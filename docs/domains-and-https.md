# Domains and HTTPS

saba routes requests to apps by hostname and serves every app over HTTPS with certificates it
obtains and renews on its own. This page covers which hostnames an app answers to and where its
certificates come from.

## The automatic domain

Every app that serves HTTP is reachable at `<app>.<base domain>` until you give it domains of
its own.

`saba server setup` sets the base domain to `<public-ip>.sslip.io` (for example
`203-0-113-10.sslip.io`). [sslip.io](https://sslip.io) is a public DNS service that answers any
name under it with the IP written in the name, so the URLs and their certificates work before you
own a domain.

To use your own, point a **wildcard** DNS record at the server and change the base domain:

```text
*.apps.example.com.   A   203.0.113.10
```

```sh
saba server config set base_domain apps.example.com
```

Existing apps move to the new names immediately. With a DNS provider configured (see
[wildcard certificates](#wildcard-certificates)), saba also obtains a single wildcard certificate
for the base domain, so new apps get HTTPS without any per-app issuance.

Apps with `internal = true`, workers and databases have no public domain at all.

## Custom domains

Create an `A` (and, for IPv6, `AAAA`) record for the hostname pointing at the server, then:

```sh
saba domains add www.example.com example.com
saba domains                       # list the app's URLs
saba domains remove example.com
```

or in `saba.toml`:

```toml
domains = ["www.example.com", "example.com"]
```

Things to know:

- **Custom domains replace the automatic one.** To keep `api.apps.example.com` as well, list it.
- A hostname can belong to only one app. Adding one that another app uses fails and names that
  app.
- `*.example.com` routes every **one-level** subdomain (`a.example.com`, not `a.b.example.com`)
  that no other app claims exactly. Configure a [DNS provider](#wildcard-certificates) for it, so
  one wildcard certificate covers every subdomain. Without one, each subdomain gets its own
  certificate on its first request, which is slower and exposed to rate limits (see
  [below](#on-demand-the-default)).
- Domain changes roll out like any configuration change, with no downtime. On a stopped app they
  are saved, and take effect on its next deploy or `saba restart` (`saba start` brings back the
  version that was stopped, as it was).
- `saba doctor` checks that each routed domain resolves, and whether it resolves to this server.

## How requests are routed

- Requests for a routed hostname go to the live replicas of its app, in round-robin order. If a
  replica refuses connections, the request is retried on the next one and the failing replica
  is skipped for two seconds.
- Plain HTTP requests for routed hostnames are redirected to HTTPS with `308 Permanent
  Redirect`, keeping the path and query. HTTPS responses carry
  `Strict-Transport-Security: max-age=31536000`.
- Apps receive `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto`. Values sent by the
  client in those headers are replaced, not trusted.
- WebSockets, server-sent events and streamed responses pass through unbuffered. HTTP/2 is
  supported for clients.
- Requests for unknown hostnames get `404`. An app with no running replica answers `503` with
  `Retry-After: 5`. A replica that does not respond results in `502`. These responses come with
  a small HTML page for browsers and an `X-Saba-Error` header.

## Certificates

### On demand (the default)

When a browser first connects to a routed hostname over HTTPS, saba requests a certificate for it
from Let's Encrypt, validates it with the HTTP-01 or TLS-ALPN-01 challenge, caches it and serves
it. The first request waits a few seconds; later ones do not. Certificates are renewed
automatically well before they expire.

Requirements:

- the hostname's DNS points at this server,
- ports 80 and 443 are reachable from the internet (check your cloud firewall).

saba only requests certificates for hostnames that are routed to an app, so random names pointed
at the server do not trigger requests to Let's Encrypt. The exception is a **wildcard route**
(`*.example.com`) without a DNS provider: then every subdomain anyone requests is routed, and
each gets its own certificate. Since anyone can make up subdomains, that can use up Let's
Encrypt's weekly limit for the domain. Configure a DNS provider for wildcard routes.

`--email` on `saba server setup` (or `saba server config set acme_email you@example.com`, which
applies after a daemon restart) gives Let's Encrypt a contact for expiry notices.

### Wildcard certificates

Let's Encrypt issues wildcard certificates (`*.example.com`) only through the DNS-01 challenge:
proving control of the domain by creating a DNS record. Give saba a way to create those records:

**Cloudflare:**

```sh
saba server config set cloudflare_api_token -     # paste the token, then Ctrl-D
```

Create the token in the Cloudflare dashboard with the **Zone → Zone → Read** and
**Zone → DNS → Edit** permissions for the zone. Passing `-` reads the token from stdin so it
stays out of your shell history; it is never shown again in full.

**Any other DNS host**, through a script on the server:

```sh
saba server config set dns_hook /usr/local/bin/saba-dns-hook
```

The hook is called as:

```text
<hook> present <record-name> <value>     # create a TXT record
<hook> cleanup <record-name> <value>     # remove it again
```

`<record-name>` is like `_acme-challenge.example.com` (no trailing dot). The hook must exit 0
on success; its output is shown if it fails. It runs as root on the server. For DNS servers that
accept RFC 2136 dynamic updates (BIND, PowerDNS, Knot and many hosted services):

```sh
#!/bin/sh
# /usr/local/bin/saba-dns-hook
set -e
action=$1 name=$2 value=$3
case "$action" in
  present) cmd="update add $name. 60 IN TXT \"$value\"" ;;
  cleanup) cmd="update delete $name. TXT \"$value\"" ;;
  *) echo "unknown action $action" >&2; exit 1 ;;
esac
printf 'server ns1.example.com\n%s\nsend\n' "$cmd" | nsupdate -k /etc/saba/tsig.key
```

Once a provider is configured, saba keeps a wildcard certificate for:

- the base domain (`*.apps.example.com`), unless it is an sslip.io or nip.io name,
- every wildcard domain routed to an app.

It checks every minute, issues what is missing, and renews 30 days before expiry (or when a third
of the lifetime is left, for short-lived certificates). Before asking Let's Encrypt to validate,
it waits until the record is visible on the zone's name servers. A failed issuance is retried
hourly, or right away after you change a DNS setting.

```sh
saba certs          # status of every wildcard and custom certificate
```

```text
ID                              NAMES                SOURCE  STATUS  EXPIRES              ISSUER
dns-wildcard.apps.example.com   *.apps.example.com   dns     active  2027-02-24 (89 days) R11
-                               *.example.com        dns     failed  -                    -
! *.example.com: creating the DNS record _acme-challenge.example.com failed: cloudflare: ...
```

Hostnames covered by a wildcard certificate use it; there is no separate on-demand issuance for
them. `saba certs` lists wildcard routes without a provider as `skipped`.

### Your own certificates

Certificates from another CA, or a Cloudflare origin certificate, can be installed for the names
they list:

```sh
saba certs add --cert fullchain.pem --key privkey.pem
saba certs remove custom-example.com --yes
```

`--cert` is the PEM chain, leaf certificate first; `--key` its private key. The certificate is
checked against the key and must list DNS names. It is stored on the server and used for those
names instead of an issued one.

saba does not renew uploaded certificates. `saba doctor` warns two weeks before one expires, and
an expired one stops being used, so on-demand issuance takes over again.

### Which certificate is served

For a routed hostname, saba picks among stored certificates (uploaded and DNS-issued) that are
currently valid and cover the name:

1. a certificate listing the exact name beats a wildcard,
2. among equals, the one that stays valid longest wins.

If none matches, the on-demand certificate is used. Requests for unrouted hostnames get no
certificate at all.

### Behind Cloudflare's proxy

With Cloudflare proxying a domain (the orange cloud), set its SSL mode to **Full (strict)** and
either install a Cloudflare origin certificate with `saba certs add`, or configure the Cloudflare
token for DNS-01. Both avoid depending on HTTP challenges passing through the proxy.

## Running without TLS

To serve plain HTTP only, for example behind another load balancer that terminates TLS, install
the daemon with `--tls off`:

```sh
saba server setup root@203.0.113.10 --tls off
```

The daemon then listens on port 80 only, and app URLs use `http://`.
