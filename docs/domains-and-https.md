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

saba manages certificates with [CertMagic](https://github.com/caddyserver/certmagic), the ACME
library behind Caddy, and stores them in `/var/lib/saba/certs/acme`. `saba certs` shows every
certificate saba serves:

```text
ID  NAMES                SOURCE     STATUS   EXPIRES               ISSUER
-   *.apps.example.com   wildcard   active   2027-02-24 (89 days)  YE1
-   *.tenants.example.com wildcard  failed   -                     -
-   www.example.com      on-demand  active   2027-02-10 (75 days)  YE2
-   shop.example.com     on-demand  pending  -                     -
! *.tenants.example.com: solving challenges: presenting for challenge: ...
```

`pending` means the certificate is requested on the first HTTPS request for that name.

### On demand (the default)

When a browser first connects to a routed hostname over HTTPS, saba requests a certificate for it
from Let's Encrypt, validates it with the TLS-ALPN-01 or HTTP-01 challenge, stores it and serves
it. The first request waits a few seconds; later ones do not. Certificates are renewed in the
background when a third of their lifetime is left, and earlier if Let's Encrypt asks for it
(ACME renewal information).

Requirements:

- the hostname's DNS points at this server,
- ports 80 and 443 are reachable from the internet (check your cloud firewall).

saba only requests certificates for hostnames that are routed to an app, so random names pointed
at the server do not trigger requests to Let's Encrypt. Under a **wildcard route**
(`*.example.com`) every subdomain is routed, and anyone can make up subdomains. Until a wildcard
certificate covers them, each subdomain gets its own certificate, at most 10 new ones per hour
per wildcard route so made-up names cannot use up Let's Encrypt's limits for the domain.
Configure a DNS provider for wildcard routes.

`--email` on `saba server setup` (or `saba server config set acme_email you@example.com`, which
applies after a daemon restart) gives Let's Encrypt a contact address.

### Wildcard certificates

Let's Encrypt issues wildcard certificates (`*.example.com`) only through the DNS-01 challenge:
proving control of the domain by creating a DNS record. Tell saba where your DNS is hosted and
give it credentials:

```sh
saba certs dns providers                                       # supported hosts and their settings
echo "$CF_API_TOKEN" | saba certs dns set cloudflare api_token=-
```

A value of `-` is read from stdin, keeping it out of your shell history. Settings are checked
before they are saved, and credentials are never shown again (`saba certs dns` prints them as
`********`). Running `set` again for the same provider changes only the settings you pass, so a
token can be rotated alone.

Built in: Cloudflare, Hetzner DNS, NameSilo and Porkbun.

```sh
# Cloudflare: a token with Zone → Zone → Read and Zone → DNS → Edit
saba certs dns set cloudflare api_token=-

# Hetzner DNS
saba certs dns set hetzner auth_api_token=-

# NameSilo
saba certs dns set namesilo api_token=-

# Porkbun: enable API access for the domain first
saba certs dns set porkbun api_key=pk1_... api_secret_key=-
```

**Any other DNS host**, through an executable on the server:

```sh
saba certs dns set hook path=/usr/local/bin/saba-dns-hook
```

It is called as:

```text
<path> present <record-name> <value>     # create a TXT record
<path> cleanup <record-name> <value>     # remove it again
```

`<record-name>` is like `_acme-challenge.example.com` (no trailing dot). The hook must exit 0
on success; its output is shown if it fails. It runs as root on the server. For DNS servers that
accept RFC 2136 dynamic updates (BIND, Knot, PowerDNS and many hosted services):

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

A newly routed wildcard domain gets its certificate within seconds. Before asking Let's Encrypt
to validate, saba waits until the record is visible on the zone's authoritative name servers. A
failed issuance is retried with growing intervals, and right away after `saba certs dns set`;
`saba doctor` reports it with the cause. The record exists only during validation.

`saba certs dns remove --yes` forgets the provider and its credentials. Wildcard certificates are
then no longer used or renewed, and hostnames under them get on-demand certificates again.

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

For a routed hostname, saba uses, in order:

1. an uploaded certificate that is valid and covers the name (one listing the exact name beats
   a wildcard; among equals, the one valid longest wins),
2. an issued certificate for the exact name,
3. a wildcard certificate covering it,
4. a new on-demand certificate.

Requests for unrouted hostnames get no certificate at all: the TLS handshake fails.

### Behind Cloudflare's proxy

With Cloudflare proxying a domain (the orange cloud), set its SSL mode to **Full (strict)** and
either install a Cloudflare origin certificate with `saba certs add`, or configure the
`cloudflare` DNS provider. Both avoid depending on challenges passing through the proxy.

## Running without TLS

To serve plain HTTP only, for example behind another load balancer that terminates TLS, install
the daemon with `--tls off`:

```sh
saba server setup root@203.0.113.10 --tls off
```

The daemon then listens on port 80 only, and app URLs use `http://`.
