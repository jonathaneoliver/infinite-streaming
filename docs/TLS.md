# TLS & Certificates

The dashboard and the per-session shaper ports serve over HTTPS by default. This
page explains the three ways to get a cert the clients accept, why the choice
hinges on **hostname**, and how to turn TLS off entirely. If you just hit a
browser cert warning, jump to [Why am I getting a warning?](#why-am-i-getting-a-warning).

## Why HTTPS at all

Plain HTTP works, but loses HTTP/2. Without HTTP/2 the dashboard's many
Server-Sent-Events streams fall under Chrome's **6-connections-per-origin** cap,
so live charts stall once enough players are open. HTTPS → HTTP/2 multiplexes
them over one connection. That's the only hard reason TLS is the default; if you
don't care about the SSE cap, plain HTTP is fine (see [Turning TLS off](#turning-tls-off)).

## The two checks every cert must pass

A cert can fail for two completely independent reasons. Keep them separate or
the modes below won't make sense:

1. **Trust** — is the cert's *issuer* in the client's trust store? Public CAs
   (Let's Encrypt) are pre-trusted everywhere. A self-signed cert or a private
   CA is trusted only on machines where you installed it.
2. **Name** — does the cert's **SAN** (Subject Alternative Name) list cover the
   exact hostname the client connected to? `dev.jeoliver.com` ≠
   `jonathanoliver-ubuntu.local` ≠ `localhost` ≠ a bare IP. Modern browsers
   ignore the legacy `CN` field entirely and require a matching SAN.

A green padlock needs **both**. Most confusing warnings are a *name* failure on
an otherwise-valid cert (see below).

## The three modes

| Mode | Hostname you use | Trusted? | Install on clients? | Covers `.local`? |
|---|---|---|---|---|
| **Plain HTTP** (TLS off) | any | n/a | nothing | n/a |
| **Self-signed / mkcert** | `.local`, IP, anything you choose | only where you installed it | the cert (self-signed) or the CA once (mkcert) | **yes** — you control the SAN |
| **Public Let's Encrypt** | a real DNS name you own (`dev.jeoliver.com`) | yes, everywhere | nothing | **no** — public CAs refuse `.local` |

The split that trips people up: a **public** CA limits you on *name* (must be a
real domain you own; never `.local`), while a **self-signed/mkcert** cert limits
you on *trust* (must be installed per-client) but lets you list **any** names —
including `.local`, the LAN IP, and a real domain all in one cert.

## The TLS toggle

`INFINITE_STREAM_TLS` (env var, default `on`) flips the whole stack between HTTPS
and plain HTTP — both the public nginx listener and go-proxy's shaper ports:

```
INFINITE_STREAM_TLS=on    # HTTPS + HTTP/2 (default)
INFINITE_STREAM_TLS=off   # plain HTTP, no cert, no HTTP/2
```

`off` is accepted as any of `off` / `0` / `false` / `no` (case-insensitive);
anything else is `on`. Implemented in `docker/launch.sh` (nginx listen opts +
cert block + the `${INFINITE_STREAM_PROXY_SCHEME}` used for nginx's
`proxy_pass` to go-proxy) and `go-proxy/cmd/server/main.go` (`ListenAndServe` vs
`ListenAndServeTLS`). The proxy-scheme piece matters: go-proxy serves TLS on
30081 when on and plain HTTP when off, so nginx's upstream scheme must track it
or every `/api/v2/*` and `/api/sessions*` route 502s. For test-dev the value is
threaded into the remote `.env` by the `make test-deploy-dev` target.

### Turning TLS off

Set `INFINITE_STREAM_TLS=off` in `.env` and redeploy. No cert is generated, no
warning ever appears, and you reach everything over `http://…`. The only cost is
HTTP/2 — heavy multi-player dashboards may hit Chrome's SSE cap. Good for quick,
cert-free local use.

## Mode 1 — self-signed (built-in default)

When `INFINITE_STREAM_TLS=on` and no cert is present, `launch.sh` auto-generates
a self-signed pair into `/media/certs/{localhost.pem,localhost-key.pem}` and
symlinks them into `/etc/nginx/certs/`. If those files already exist it uses them
untouched — that's the hook the mkcert and LE paths below rely on.

**Trust:** nothing trusts a self-signed cert until you install *that exact cert*
on each client. Regenerate it (new key/host/SAN) and you re-install everywhere —
there's no chain, the cert is its own issuer.

**Name:** the SAN comes from the **`INFINITE_STREAM_TLS_SAN`** env var, so each
deployment covers its own hostnames. It's a comma-separated openssl SAN list —
default `DNS:localhost,IP:127.0.0.1`. Set it in `.env` to every name/IP clients
reach the box by, for example:

```
INFINITE_STREAM_TLS_SAN=DNS:dev.jeoliver.com,DNS:jonathanoliver-ubuntu.local,DNS:localhost,IP:192.168.1.50
```

A self-signed cert can list **all** of those at once — you're your own CA, so no
one checks domain ownership. (Trust is still per-client; SAN only fixes the
name half.) The cert's `CN` is set to the first `DNS:` entry for readability,
but browsers ignore it.

**Regeneration:** `launch.sh` records the SAN it generated in
`/media/certs/.self-signed-san`. Change `INFINITE_STREAM_TLS_SAN` in `.env` and
redeploy — because the recorded SAN no longer matches, the cert is regenerated
with the new names. A **manually supplied** cert (mkcert / LE) has no marker
file, so it is never regenerated over.

## Mode 2 — mkcert (self-signed, but with a trusted local CA)

mkcert is the ergonomic upgrade over plain self-signed. It splits the job in two:

1. Creates a **local root CA** once (`rootCA.pem` / `rootCA-key.pem`) and installs
   that CA into your OS + browser trust stores via `mkcert -install`.
2. Issues **leaf** server certs *signed by that CA*.

Because clients trust the **CA**, they trust any leaf it signs — so you can
regenerate server certs forever and never touch the clients again, as long as the
CA stays installed.

| | Plain self-signed | mkcert |
|---|---|---|
| Trust chain | leaf signs itself | local root CA → leaf |
| Install on clients | the cert, every time it changes | the CA, once |
| SAN handling | manual `-addext subjectAltName` | pass names; builds proper SAN + `serverAuth` EKU |
| Blast radius if key leaks | vouches only for itself | CA key can mint a cert for **any** domain → guard `rootCA-key.pem` |

### Using mkcert with this server

On a Mac that has mkcert installed:

```bash
mkcert -install                                            # trust the local CA on this machine
mkcert jonathanoliver-ubuntu.local localhost 127.0.0.1     # multi-SAN leaf
```

Copy the resulting `*.pem` (cert) and `*-key.pem` (key) to the host's cert dir as
`localhost.pem` / `localhost-key.pem`. The test-dev compose override already wires
this up — `tests/deploy/override-dev.yml` bind-mounts `./certs → /media/certs:ro`,
and `launch.sh` skips its self-signed fallback when those files exist, so the
mounted mkcert pair wins. For k3d, drop them in `$K3S_CERTS_DIR`.

Every client that should get a green padlock needs mkcert's **CA** installed —
that's the catch, and it's why AppleTV is painful (see [AppleTV](#appletv-and-tvos)).

## Mode 3 — public Let's Encrypt via Cloudflare DNS-01

This is the cert currently on test-dev: a real Let's Encrypt cert for
`dev.jeoliver.com`. "Issued By: R12 / Let's Encrypt" in a browser confirms it —
**Cloudflare did not sign it**; Cloudflare is only the DNS provider used to solve
the ACME challenge.

### How it was issued (recoverable runbook)

The cert was obtained with the **DNS-01** challenge, which proves domain control
by publishing a TXT record — no inbound port 80/443 needed, so it works for a box
that isn't WAN-reachable.

1. **Own the domain on Cloudflare.** `jeoliver.com` is hosted on Cloudflare DNS.

2. **Publish an A record** for the subdomain pointing at the box's LAN IP, DNS-only
   (grey cloud, no proxy):

   ```
   dev.jeoliver.com  A  192.168.1.50
   ```

   This is a *public* DNS record holding a *private* IP — see
   [No WAN exposure](#no-wan-exposure) for why that's safe.

3. **Issue the cert with `lego`** (Go ACME client) using the Cloudflare DNS plugin.
   Create a scoped Cloudflare API token (Zone → DNS → Edit on `jeoliver.com`) and:

   ```bash
   export CLOUDFLARE_DNS_API_TOKEN='<scoped-token>'
   lego --email you@example.com \
        --dns cloudflare \
        --domains dev.jeoliver.com \
        --path /tmp/lego \
        run --accept-tos
   ```

   `lego` writes the TXT record via the Cloudflare API, waits for propagation,
   completes the challenge, and drops `dev.jeoliver.com.crt` (fullchain) +
   `dev.jeoliver.com.key` under `/tmp/lego/certificates/`.

   > **Security:** the token can edit `jeoliver.com` DNS. Export it for the run
   > and clear it after; never commit it. If a token ends up in shell history or
   > a transcript, **revoke it** in the Cloudflare dashboard.

4. **Install the cert** on the host as the server's `localhost.pem` (fullchain) and
   `localhost-key.pem` (key) — same slots the self-signed/mkcert paths use.

5. **Point the announce URL at the matching name** (see [Announce URL](#the-announce-url-must-match-the-cert)):

   ```
   INFINITE_STREAM_ANNOUNCE_URL=https://dev.jeoliver.com:21000
   ```

6. **Renew** — LE certs last 90 days. Cron `lego renew` (weekly) and reinstall.
   `acme.sh` or `certbot --dns-cloudflare` are equivalent alternatives; `lego` was
   chosen as a single static binary.

### No WAN exposure

`dev.jeoliver.com` resolves to `192.168.1.50` — an RFC1918 LAN address — in
**public** DNS. That sounds alarming but isn't:

- **On the LAN:** your machine resolves the name to `192.168.1.50` and connects
  directly over the local network. The cert name matches → green padlock. The WAN
  is never involved.
- **From the internet:** the same lookup returns `192.168.1.50`, which is
  non-routable on the public internet. There's no port-forward and no firewall
  hole — the box is invisible from outside.

The only "public" thing is the DNS *record*; the IP it hands out goes nowhere off
your LAN. Side effect: it publishes your internal IP (harmless), and the name only
*works* for clients sitting on the `192.168.0.x` network.

## The announce URL must match the cert

`INFINITE_STREAM_ANNOUNCE_URL` is what the rendezvous / "Pair a TV" flow hands to
clients. **It must use a hostname that's in the cert's SAN**, or every TLS client
hits the name-mismatch warning:

| Serving this cert | Announce as | Why |
|---|---|---|
| LE `dev.jeoliver.com` | `https://dev.jeoliver.com:21000` | only SAN in the cert; LAN clients resolve it via public DNS |
| mkcert/self-signed with `.local` SAN | `https://jonathanoliver-ubuntu.local:21000` | matches SAN; mDNS resolves it on the LAN with zero DNS setup |
| TLS off | either name over `http://` | no cert, no name check |

You can't have one **public** cert that's clean for both `dev.jeoliver.com` *and*
`jonathanoliver-ubuntu.local` — a public CA won't sign `.local`. If you need both
names clean, that's a self-signed/mkcert multi-SAN cert (and the CA installed on
clients). Note the Server-Info QR derives its URL from `window.location`, so it
mismatches if you *manually* browse by a name not in the cert; the announce URL
only governs the pairing broadcast.

## Why am I getting a warning?

The common case: **a valid cert, wrong name.** Example — the browser shows "not
secure" for `https://jonathanoliver-ubuntu.local:21000`, but the Certificate
Viewer says the cert is a real Let's Encrypt cert for `dev.jeoliver.com`. That's
purely a name mismatch: the cert is trusted and fine, you just reached it by a
name the cert doesn't list. Browsers let you click through; strict clients don't —
Go/curl fail hard with:

```
x509: certificate is valid for dev.jeoliver.com, not jonathanoliver-ubuntu.local
```

Fixes, in order of cleanliness:

- **Reach it by the name in the cert** — `https://dev.jeoliver.com:21000`. Zero
  install, instant green padlock (works for LAN clients; see [No WAN exposure](#no-wan-exposure)).
- **Reissue the cert with the name you want in the SAN** — mkcert if you want
  `.local`; only a public CA can give a warning-free *real* domain.
- **Turn TLS off** for cert-free local use.

If the warning is about the *issuer* (not the name) — "self-signed", "unknown
authority" — that's a **trust** failure: install the cert (self-signed) or the CA
(mkcert) on that client.

## AppleTV and tvOS

tvOS has no Settings UI to install a profile from a file, and no browser — and an
AppleTV 4K (2nd gen) has no USB port. A custom cert or CA must be delivered as a
**configuration profile** (`.mobileconfig`) over the network. Two tools, both
confusingly named "Configurator":

- **Apple Configurator (iPhone app):** tap **Add → Apple TV**, **hold the phone
  near the AppleTV**, enter the pairing code shown on the TV, then push the
  `.mobileconfig` containing your CA. The cleanest wireless path for a portless TV.
- **Apple Configurator 2 (Mac app):** pairs with the AppleTV over Wi-Fi/Bonjour
  (Paired Devices → 6-digit code on the TV; needs an Apple ID signed into the app,
  both devices on the same SSID/VLAN), then drag the `.mobileconfig` onto its tile.
- **MDM** enrollment pushing the CA profile (overkill for home/dev).

Either way the AppleTV is the highest-friction client — discovery/pairing is
flaky across VLANs and tvOS versions. That friction is what pushed test-dev toward
the public LE cert: a publicly-trusted cert needs **nothing** installed on the TV.
The tradeoff is the LE cert only covers the real domain, so the TV must reach the
box by `dev.jeoliver.com` (and therefore be on the LAN where that name resolves).

To build the `.mobileconfig` wrapping an mkcert CA:

```bash
CA_PEM="$HOME/Library/Application Support/mkcert/rootCA.pem"
B64=$(base64 -i "$CA_PEM" | tr -d '\n')
# Wrap $B64 in a Configuration Profile plist with a com.apple.security.root
# payload, then validate with: plutil -lint out.mobileconfig
```

iOS/web players are more forgiving: ATS exceptions
(`NSAllowsLocalNetworking` / `NSExceptionAllowsInsecureHTTPLoads`) relax *cleartext*
and some TLS strictness, which is why streaming sometimes "works" over a
mismatched `.local` name — but ATS exceptions do **not** disable cert hostname
validation in general, so don't rely on it.

## Choosing a mode

- **Just developing locally, don't care about the SSE cap** → `INFINITE_STREAM_TLS=off`.
- **Want HTTP/2 + green padlock on your own machines, reachable by `.local`** →
  mkcert, install the CA on each client.
- **Need AppleTV / many clients with zero per-client install** → public LE cert,
  reach the box by its real domain on the LAN.

See issue #484 for the proposal to wire these into first-class `make` targets
(`make ssl-mkcert` / `make ssl-letsencrypt`).
