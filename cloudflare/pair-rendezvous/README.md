# InfiniteStream pairing rendezvous

A ~80-line Cloudflare Worker + KV namespace that lets the TV reference
clients (Apple TV, Android TV) discover a server URL via a 6-digit
pairing code without any typing on the TV.

## Flow

1. TV app starts → no server configured → generates a code (`ABC123`)
   and shows it: *"Open InfiniteStream dashboard on your phone and
   enter ABC123."* It then polls `GET /pair?code=ABC123` every ~2s.
2. User opens the dashboard on their phone, types the code into the
   "Pair Device" field in Server Info, taps Pair. The dashboard does
   `POST /pair?code=ABC123 {server_url: window.location.origin}`.
3. The Worker stores the URL in KV with a 10-minute TTL.
4. The TV's next poll returns the URL → it adds the profile, connects,
   plays. Then `DELETE /pair?code=ABC123` to release the slot.

The server URL transits Cloudflare KV briefly (tens of seconds at most)
and is then deleted. No persistent storage of which server you have.

## Same-WAN check

By default the rendezvous compares the publisher's public IP (the
phone/dashboard) with the consumer's public IP (the TV) using the
`CF-Connecting-IP` header. If they differ it returns **403** —
prevents a phone on a different network from publishing a URL to a TV
on yours (which usually wouldn't reach the URL anyway, but this also
guards against cross-network mischief if someone learns your code).

Disable for unusual setups where the TV and dashboard egress different
public IPs (e.g. TV on home wifi, phone on cellular):

```sh
wrangler secret put RENDEZVOUS_ALLOW_CROSS_NETWORK
# enter the value: 1
```

Or set in `wrangler.toml`:

```toml
[vars]
RENDEZVOUS_ALLOW_CROSS_NETWORK = "1"
```

## Deploying your own

You don't have to use a shared rendezvous. The Worker is small enough
that you should run your own.

```sh
cd cloudflare/pair-rendezvous
npm install -g wrangler
wrangler login
wrangler kv namespace create PAIRING
# Copy the printed `id = "..."` into wrangler.toml [[kv_namespaces]]
wrangler deploy
```

Wrangler prints the deployed URL (e.g. `https://infinitestream-pair.your-account.workers.dev`).
Optionally bind a custom domain like `pair.your-domain.com` under
**Workers → Settings → Triggers**.

## Pointing the apps at your rendezvous

The clients default to a public rendezvous URL hardcoded in the source.
Override per-deployment via env / build flag:

| Surface | How to override |
|---|---|
| Dashboard JS | `window.ISMRendezvousURL = '...'` before `shared-nav.js` loads, or build-time replace |
| iOS / tvOS | `INFINITE_STREAM_RENDEZVOUS_URL` Info.plist key |
| Android TV | `BuildConfig.RENDEZVOUS_URL` |

If you don't want to host the rendezvous at all, just don't enter codes —
the rest of the dashboard / app works fine without pairing.

## Free-tier sizing

Cloudflare's free tier allows:
- 100k Worker requests / day (~1.1 req/s sustained)
- 1k KV writes / day, 100k reads / day, 1 GB storage
- Rendezvous pairing is one POST + a handful of GET polls + one DELETE
  per pairing event → comfortably hundreds of pairings/day on free tier.
