# Headless harness

Everything here runs without a phone or a human (SPEC §7.2). Status on
2026-09-05: `dialler`, `asterisk`, and the `sipp` conformance suite build and
pass on Apple silicon (Docker Desktop 29). The baresip phones are the next
piece to verify. Each piece is independent so failures are easy to isolate.

Things learned the hard way: Debian 12 dropped Asterisk (Alpine packages it);
Debian's `sip-tester` is built without OpenSSL (SIPp is compiled from source
here); Debian's baresip is 1.x from 2020 with no WAV recorder (baresip 3.x is
compiled from source here); the `/data` volume must be pre-owned by the
distroless non-root uid; the test tone must be stereo because the phones
negotiate `opus/48000/2` on the wire (the account lists it as
`opus/48000/1`: the phones run mono Opus, and baresip names the codec by its
audio channel count).

## Engine-spike call test

`make spike-call` runs [`spike/call_test.sh`](spike/call_test.sh): the diago
B2BUA from [`../spike`](../spike) plus two baresip phones on their own compose
file ([`docker-compose.spike.yml`](docker-compose.spike.yml)). Phone 201
dials 212 via baresip's `ctrl_tcp`, the B2BUA bridges the legs with media
proxied through itself, and the callee's recording is asserted to contain
audio. Passing on 2026-09-05 (callee RMS ≈ 6500 against a threshold of 200).

## Layout

| Path | Role |
|---|---|
| `dialler/` | Builds `dialler-server` from `../server` into a distroless image |
| `asterisk/` | Alpine-packaged Asterisk (Debian 12 dropped it) as the customer PBX peer: desk phone `100` (G.722 first, G.711 fallback), trunk peer `dialler` |
| `sipp/` | App-leg conformance: REGISTER over TLS succeeds, plain TCP is refused, INVITE returns 501 until the B2BUA exists |
| `baresip/` | Two headless phones (`211`, `212`) registering over TLS with WAV audio in/out |
| `provision.sh` | Enrols the two phones and seeds the directory via the admin API |
| `call_test.sh` | End-to-end call 211 → server → 212 with media asserted (`make harness-call`) |
| `wake_test.sh` | Callee starts with no SIP UA; `fake-app` on the gateway receives the wake, creates the UA via `uanew`, bridge completes, media asserted (`make harness-wake`) |
| `innet.sh` | Runs a repo script inside the compose network, for hosts that cannot reach published localhost ports |
| `flow_gone_test.sh` | Callee registered then SIGKILLed (no clean unregister): the server must detect the dead connection and take the wake path, never dial the stale route (`make harness-flow-gone`) |
| `probe_call.sh` | **NAT regression** (`make probe-call`, macOS host only): the real baresip engine on this Mac registers through Docker's port forwarding, a genuine NAT, and 212 calls it. `DIALLER_REWRITE_CONTACT=false make probe-call` must fail. |

Why the NAT case needs the host: the container phones bind their outbound
TLS connection to their listening port, so their advertised Contact equals
their source address and the server's connection pool finds them either way.
A router container does not help on Docker Desktop, which routes between
its networks at the VM level and bypasses it. Docker's published ports are
the one real NAT available, so the regression test uses the engine probe on
the Mac. This is the bug that reached a real phone on 2026-09-06.

## Addresses and ports

`make harness-up` advertises this Mac's en0 address (`DIALLER_PUBLIC_HOST`)
to phones for SIP and media, and publishes 7443 (signal), 5061 (SIP/TLS),
8080 (HTTPS, directory/admin) and UDP 20000–20100 (relayed media) on the host. The docker
phones reach the relay through the same published ports, so one setting
serves both real devices and the container tests.

## Bring-up

```sh
python3 harness/baresip/media/gen_tone.py                       # once: creates in.wav (an aperiodic burst pattern: the quality gates correlate recordings against it)
# Next to a running `make dev-server`: move the published ports and pin the
# public host, e.g. DIALLER_PUBLIC_HOST=dialler DIALLER_SIGNAL_HOSTPORT=7444
# DIALLER_SIP_HOSTPORT=5063 DIALLER_HTTP_HOSTPORT=8081 DIALLER_RTP_MIN=20200
# DIALLER_RTP_MAX=20300 make harness-…  (the Mac-address default would send
# a phone's responses to the native server on 5061).
# Identities: dev-ha/211 and dev-hb/212 are the docker phones; the simulator
# harness (sim_call.sh) and the Mac engine probe are dev-s/203; dev-a/201 is
# the REAL app's and no test dials it (ring_native/ring_sim do, on purpose),
# so a real phone pointed at this server never takes a test's calls.
docker compose -f harness/docker-compose.yml up --build -d dialler asterisk
sh harness/provision.sh                                          # prints device tokens
docker compose -f harness/docker-compose.yml --profile test run --rm sipp
docker compose -f harness/docker-compose.yml --profile test up baresip-a baresip-b
```

Server logs (`docker compose logs -f dialler`) show `sip register user=211`
when a baresip phone binds.

## What is deliberately missing until the Phase 0 engine spike

- **INVITE handling / B2BUA.** The registrar answers 501. `invite_refused.xml`
  pins that so the switch to real call setup is a deliberate scenario change.
- **Trunk-side listener.** Asterisk's `dialler` peer points at `dialler:5060`
  over TCP, which nothing listens on yet. Asterisk will log the peer as
  unreachable; that is expected.
- **SIP Digest on the app leg.** Registration is accepted for any provisioned
  user over TLS. This must land before the public edge (Phase 4b).
- **Media assertions.** `baresip-b` records to `/media/out-212.wav`; the test
  that plays `in.wav` from A and asserts on B's recording needs the B2BUA.
