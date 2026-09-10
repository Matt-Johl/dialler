# Standalone PBX for real phones (Ubuntu)

The Docker harness's Asterisk sits behind Docker Desktop's UDP port proxy,
which does not carry two-way RTP for a phone on the LAN (the same fault
that silenced the iPhone against the containerised server), and Homebrew
no longer ships Asterisk for the Mac. For a physical SIP phone, the PBX is
a stock Ubuntu Asterisk on another machine on the LAN, configured from
the files in this directory:

```sh
# on the Mac
scp -r harness/asterisk-native ubuntu-box:~/
ssh ubuntu-box 'sudo DIALLER_HOST=<this Mac's LAN address> sh asterisk-native/install-ubuntu.sh'

ASTERISK_HOST=<ubuntu-box address> make dev-server     # the light server, trunked to it
```

`install-ubuntu.sh` installs the `asterisk` package if needed, keeps the
package's original `/etc/asterisk` as `/etc/asterisk.dist`, renders
`pjsip.conf` with the light server's address, copies the other configs
in, opens the ports in ufw when it is active, and restarts Asterisk.
Re-run it after editing any `.conf` here.

Then in the app, dial `101` (the phone), `600` (PBX echo) or `echo`
(server echo); from the phone dial `201` (the app) or `600`.

## Phone configuration (Grandstream and similar)

| Setting | Value |
|---|---|
| SIP server / registrar | the Ubuntu box's LAN address, port 5060 |
| Transport | UDP |
| SIP user ID / auth ID | 101 |
| Password | dialler101 (in `pjsip.conf`) |
| Codecs | PCMU, PCMA (G.711); disable others |
| NAT traversal / STUN | off (same LAN) |

`sudo asterisk -rvvv` on the Ubuntu box opens the console; `pjsip show
endpoints` lists 101 as Avail once the phone has registered, and the
`dialler` endpoint as Avail once `make dev-server` is up on the Mac (it is
qualified every 10 s).

## Transfers

When the app transfers a PBX phone to another PBX extension (say 101 is
talking to the app and the app transfers it to 600), the light server hands
the transfer to Asterisk with a REFER on the trunk leg and drops out: the
audio then runs phone ↔ Asterisk ↔ 600 without the server. That needs
`allow_transfer=yes` on the `dialler` endpoint (pjsip's default, stated in
`pjsip.conf`) and the target reachable in the `from-dialler` context. If a
PBX refuses the REFER, the server completes the transfer itself as before,
which keeps it in the media path; on a CUCM trunk, enable REFER on the
trunk profile to get the offload.

## Ports

Ubuntu box: 5060/udp (SIP), 10000–10200/udp (RTP, `rtp.conf`).
Mac: 5061/tcp+tls and 7443/tls (light server, app side), 5062/udp (the
server's trunk listener), 20000–20100/udp (server media relay).
