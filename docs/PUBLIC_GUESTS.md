# Public guests (no Tailscale) [SL-P0-1]

A guest with only a browser joins through **Tailscale Funnel**. Signaling, the
page and uploads go over Funnel :443. Media tries direct UDP first. When UDP is
blocked, it falls back to the embedded TURN relay, which is reached as
`turns:` through Funnel :8443.

## Host setup

```sh
# 1. Run the daemon (loopback HTTP, loopback TURN, SFU UDP mux on 50000).
p2pvideod --public-url https://studio.<tailnet>.ts.net --funnel-turn

# 2. App (HTTP + WS + upload) on Funnel :443.
tailscale serve  --bg --https=443 http://127.0.0.1:7443
tailscale funnel --bg --https=443 on

# 3. TURN over TLS on Funnel :8443. Tailscale terminates TLS and forwards plain TCP.
tailscale serve  --bg --tls-terminated-tcp=8443 tcp://127.0.0.1:3478
tailscale funnel --bg 8443 on
```

Check with `tailscale serve status` and `tailscale funnel status`. Only :443 and
:8443 should be listed, and :8443 must point at `tcp://127.0.0.1:3478`.

Optional, for direct UDP (tier B1) without relying on NAT hole-punching:
forward UDP 50000 on the router to the host. srflx candidates from public STUN
usually work without this.

### First time: Tailscale asks you to allow Funnel

On a tailnet where Funnel isn't enabled yet, `tailscale funnel` prints

```
Funnel is not enabled on your tailnet.
To enable, visit:

         https://login.tailscale.com/f/funnel?node=<id>
```

and waits until the tailnet owner approves it (Serve/HTTPS has the same prompt with
`/f/serve` or `/f/https`). From a terminal: open the link, approve, and the command
continues. In the Mac app (Settings → Guest Access or Setup → How guests join → Let
guests join from anywhere), p2p.video reads the command's output as it runs, stops
waiting as soon as the link appears, and shows **Allow public links in Tailscale** with
**Open Tailscale** (opens that page in your browser) and **Try Again** (runs the same
commands again after you approve). Nothing after the waiting command runs until then.
See `docs/DESKTOP_APP.md` → Approval links.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--udp-port` | `50000` | SFU single UDP mux port, bound on every interface (including loopback). `0` = ephemeral ports. If the port is busy, the daemon logs a warning and falls back to ephemeral ports. |
| `--no-public-stun` | off | Don't advertise public STUN to clients and don't use it in the SFU (ADR-005). Public guests then rely on TURN. |
| `--turn-listen` | `127.0.0.1:3478` | Embedded TURN TCP listener. Must be loopback. Empty disables TURN. |
| `--funnel-turn` | off | Advertise `turns:<public-url host>:8443?transport=tcp`. Needs an https `--public-url`; conflicts with `--no-funnel`. |
| `--turn-url` | — | Extra comma-separated TURN URLs to advertise, e.g. `turn:127.0.0.1:3478?transport=tcp` for local testing. |

The relay always runs unless `--turn-listen ""` is passed. It's only
**advertised** when `--funnel-turn` or `--turn-url` is given.

## What the guest receives

Only after admission (never while pending, never when denied):

```json
{"iceServers":[
  {"urls":["stun:stun.cloudflare.com:3478","stun:stun.l.google.com:19302"]},
  {"urls":["turns:studio.<tailnet>.ts.net:8443?transport=tcp"],
   "username":"<expiryUnix>:<participantId>","credential":"base64(HMAC-SHA1(secret, username))"}]}
```

- Credentials last 6 h. The secret is 32 random bytes per daemon run, kept in
  memory and never logged. After a daemon restart, clients re-admit and receive
  new credentials.
- On every TURN request the relay also checks that the participant is still
  admitted (or briefly disconnected). Once a participant is denied or removed,
  their credentials stop working immediately.

## Relay design

- Relay sockets are bound to **127.0.0.1** (a static relay address generator)
  and given to the client as its relay candidate. The SFU advertises a loopback
  host candidate on its mux port, so the relay reaches the SFU over loopback.
  Relay traffic never leaves the host, and no public relay port is opened.
- Relayed datagrams are filtered. Peer permissions are loopback-only, and each
  datagram can only go to the SFU mux port. If the mux fell back to ephemeral
  ports (port busy or `--udp-port 0`), TURN is **not started or advertised**
  and the daemon logs a warning: there is no fixed port to pin relay traffic
  to (SL-SEC-1, L2). RFC 6062 TCP relay allocations are refused.
- Denying or removing a participant, or ending the meeting, closes that
  participant's live relay allocations at once (L1), not at their next refresh.
- The SFU gathers srflx candidates through public STUN (when allowed) on
  ephemeral sockets, which is pion's behaviour. Host candidates use the single
  mux port.

## Security boundary

Through Funnel, guests can load the page, use the guest WebSocket (invite
token) and upload chunks (participant grant). Everything else is refused when
a request has `Tailscale-Funnel-Request` set:

- host bootstrap and management (`requireHost`, `managementAllowed`), and
- a host-token WebSocket (host control: admit, deny, record). It returns 403.

Since SL-SEC-1 (`docs/SECURITY.md`) the public path is further bounded:

- A host token is honoured only from a management position (local, or the
  configured owner over private Serve), for HTTP and WebSocket alike. A host
  token used through Funnel or from another tailnet device is refused.
- An invite or room link shows only the waiting-room view of
  `GET /api/v1/sessions/{id}` (title and recording settings). Names need an
  admitted participant's grant; takes and the full roster need the host.
- Join attempts are rate-limited per client address (the first
  `X-Forwarded-For` entry Funnel/Serve sets) and per session, at most 20 people
  can wait at once (`busy` after that, no database row), and a socket that
  never sends `hello` is closed after 10 s.
- Rotating or revoking a room link, or re-issuing an invite, denies everyone
  still waiting under the old one.
- Uploads are bounded per chunk, per sequence number, per track (a quota from
  the take's length) and per participant (tracks per take); a slow upload
  cannot stall anyone else's.
- `/api/v1/tailscale/status` (tailnet name and IPs) is host-only.

Tests: `TestFunnelGuestJoinsWhileManagementStaysDenied`,
`TestPrivateServeOwnerManagement`, `TestManagementAuthorization`, and the
`TestM*`/`TestL*` regression tests listed in `docs/SECURITY.md`.

## Verification

- `go test ./internal/turn`: credential mint and verify (expiry, wrong secret,
  re-labelled participant), and a real TURN-over-TCP allocation. It relays to
  the allowed loopback peer only.
- `go test ./internal/sfu -run RelayOnly`: a pion peer with
  `ICETransportPolicyRelay` connects to the SFU only through the embedded relay.
  The selected local candidate is relay and the remote is the loopback mux port.
- `web/tests/relay.browser.mjs`: a Chromium guest is forced to
  `iceTransportPolicy: 'relay'`, and remote video decodes on both sides. The
  guest's selected pair is relay (TCP) to the SFU mux port, and the credential
  isn't in the daemon logs.
- **Not yet verified:** a real Funnel :8443 `--tls-terminated-tcp` run from an
  outside network (spike S1). The browser test covers `turn:` over plain TCP on
  loopback, which is the same server path minus Tailscale's TLS termination.
