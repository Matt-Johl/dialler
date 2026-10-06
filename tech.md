# Dialpark and Dialler: how the system works

This document describes the whole product: what it does, what its parts are, how a call travels, how
phones are set up and managed, how it is licensed, and where its limits are. It is written for someone
who is not a software developer but needs to understand the system completely, for example to sell it,
support it, install it or evaluate it. Numbers and names are taken from the software as built, not
from plans.

Contents

1. What the product is
2. The two names
3. The parts
4. How a phone is reached when the app is closed
5. Ports, addresses and where things run
6. How calls work, step by step
7. Audio and encryption between phone and server
8. Connecting to a PBX
9. Setting up a phone: enrolment
10. The Dialler app, screen by screen
11. The admin console, page by page
12. Directories
13. Diagnostics and logs
14. The licence
15. Data, backup, certificates, restarts
16. Security model
17. How the product is tested
18. Limits and things that are not built
19. Third-party components
20. Glossary

---

## 1. What the product is

Dialpark turns iPhones into extensions of a company's phone system, on the company's own network,
without any cloud service. A server on the site connects the existing SIP phone exchange (the PBX) to
iPhones running the Dialler app. The server wakes a phone when a call arrives, so the phone rings even
when the app is closed or the phone is locked. Nothing goes through Apple's push service or the
internet: if the internet is down but the site's Wi-Fi is up, calls still work.

The product can also run with no PBX at all. In that case the Dialpark server is the exchange, and the
phones call each other through it.

In one paragraph: the PBX sends a call to the Dialpark server. The server tells the phone a call is
coming. iOS shows the normal full-screen incoming call. When the person answers, the app connects to
the server and the server joins the two halves of the call, carrying the audio between them.

What it is not: it is not a remote-working product. Phones receive calls only while they are on the
Wi-Fi networks the administrator has assigned to them. Away from those networks, on cellular or at
home, the phone is unreachable for incoming calls. This is a deliberate choice, explained in section 4.

## 2. The two names

- **Dialpark** is the system: the call server, the web console used to manage it, the licence, and
  everything that runs on the company's machine. "A Dialpark server" is the thing a phone connects to.
- **Dialler** is the iPhone app, installed from the App Store. A phone is "enrolled to a Dialpark
  server". The app's name, its App Store listing and its identity on the phone are Dialler.

When this document says "the server", it means the Dialpark server. When it says "the app", it means
Dialler.

## 3. The parts

There are five pieces of software. Three run on the company's machine, two run on each iPhone.

**dialpark-server** is the call server. It is a single program with no external dependencies. It
contains the registrar and call controller for the phones, the media relay that carries audio, the
component that wakes phones, the routing between phones and the PBX, the per-phone directories, the
enrolment and credential store, the diagnostics intake, the licence check and the admin API. It keeps
no call history.

**dialpark-admin** is the web console the administrator uses. It is a second program that talks only
to the call server's admin interface. It has one operator account and no state of its own beyond the
password and who is signed in, so it can be restarted or replaced without affecting a single call.

**dialpark-licence** is the vendor's tool for creating licences. It never runs at a customer site and
needs no network.

**Dialler** is the iPhone app: the keypad, directory, recents and settings, the in-call screen, and the
telephone engine that speaks SIP and carries audio.

**The Dialler push provider** is a small extension inside the app. iOS runs it in the background while
the phone is on an assigned Wi-Fi network. It holds a permanent encrypted connection to the server,
carries only signalling, never audio, and hands an incoming call to iOS so the phone can ring with the
app closed.

The server and console are written in Go and ship as single files. The app is written in Swift and
uses the open-source baresip telephone engine, compiled in (see section 19).

## 4. How a phone is reached when the app is closed

This is the central idea and the reason the product exists, so it is worth understanding properly.

### The problem

iOS suspends apps that are not on screen. A suspended app cannot keep a network connection open, so a
conventional SIP softphone stops receiving calls within seconds of leaving the foreground. The usual
answer is Apple's Push Notification Service: a cloud service that wakes the app. That requires internet
access and a path from the company's phone system to Apple's servers, which many sites cannot or will
not provide.

### Apple Local Push Connectivity

Apple provides one other sanctioned way to wake an app, designed for exactly this situation: Local Push
Connectivity. An app may contain an "app push provider" extension. The administrator lists the Wi-Fi
network names the site uses. While the phone is joined to one of those networks, iOS keeps the
extension running in the background, with the app itself closed or suspended. The extension keeps its
own connection to a local server. When a call arrives, the extension tells iOS, and iOS launches the app
and rings the phone through the standard call interface, exactly as a cellular call would.

Dialpark uses this and nothing else. There is no Apple push fallback. The consequences are:

- **Wi-Fi bound.** Background calls arrive only on the listed networks. Several network names can be
  listed, so a site with more than one Wi-Fi name is fine. Moving from one listed network to another
  restarts the extension and the phone re-registers from its new address.
- **Off the listed networks, no background calls.** On cellular, at home, or on a visitor network, the
  phone is unreachable until the app is opened. This is the accepted scope.
- **Apple entitlement.** The app needs Apple's Network Extension permission for push providers, which
  Apple grants for on-site use cases. The App Store build carries it.
- **No cloud at all.** The vendor runs no service. The company's server is the only thing a phone ever
  talks to.

### Three states of the app

The way a call reaches the phone depends on what the app is doing. In all three cases the same server
and the same messages are involved; only who holds the connection differs.

| App state | Who holds the connection to the server | What happens on a call |
|---|---|---|
| Open, on screen | The app itself | The app is already registered for calls. The server sends the call directly and also sends a wake-up message. Whichever arrives first rings; the two are recognised as one call. |
| In the background or closed, on a listed Wi-Fi | The push provider extension | The extension receives the wake-up, hands it to iOS, iOS starts the app and shows the incoming call. The extension tells the server "will answer". When answered, the app registers and the server connects the call. |
| In the background or closed, not on a listed Wi-Fi | Nobody | The server cannot deliver the wake-up. The caller hears the "unavailable" result at once. |

### The wake-up connection in detail

The server listens on port 7443. The app and the extension connect to it over TLS 1.3 and exchange
short JSON messages. The first message from the phone identifies the device and proves its credential.
The server answers with a welcome that carries everything the phone needs: its SIP account details,
the directory version, the assigned Wi-Fi names, and the licence (section 14).

The server sends a heartbeat every 10 seconds and the phone answers. Either side treats 30 seconds of
silence as a dead connection. The server drives the heartbeat rather than the phone, because iOS may
not give the extension processor time on its own, but incoming data always wakes it. An earlier design
where the phone drove it produced hundreds of reconnections in one night.

A wake-up message names the call, the caller, and a deadline. If the phone reconnects while a call is
still ringing, the server sends the wake-up again, and it also replays any cancellation it issued in
the meantime, so a phone never rings for a caller who has already hung up. A phone that loses its
connection never treats that as a call event.

If the server restarts or the connection drops, the extension reconnects immediately, then after 1, 2
and 3 seconds, then every 5 seconds for ever. The app in the foreground reconnects after 0, 1, 2, 4, 8
and 15 seconds. A phone is therefore back within a few seconds of a server restart, with no action by
anyone.

### Assigning Wi-Fi networks

The administrator sets the Wi-Fi names per phone in the console (up to 32 names). The server sends
them to the phone in the welcome and again whenever they change, and the app applies them to iOS. An
identical list is never re-applied, because re-applying restarts the extension. An empty list removes
background calls for that phone. Only the app can apply the setting; if the app is closed when the list
changes, it applies it the next time it is opened.

## 5. Ports, addresses and where things run

Everything runs on one machine on the company's network, normally a small server or virtual machine.
The phones must be on a network from which they can reach that machine.

| Port | Used for | Who connects | Default binding |
|---|---|---|---|
| 7443 TCP | Wake-ups and signalling (TLS 1.3) | Phones (app and extension) | All interfaces |
| 5061 TCP | SIP over TLS for calls | Phones | All interfaces |
| 8080 TCP | Enrolment, directory and diagnostics over HTTPS | Phones | Loopback by default; a real deployment binds it on the network |
| 20000 to 20100 UDP | Call audio, relayed by the server | Phones and the PBX | All interfaces |
| 5060 UDP/TCP, or 5062 TLS | Calls from the PBX (trunk mode only) | The PBX | All interfaces |
| 8081 TCP | The admin API | The console only, normally on the same machine | Loopback |
| 8443 TCP | The admin console in a browser | The administrator | Loopback by default |

The server advertises one address to phones, its "public host", which defaults to the machine's first
network address. The same address appears in the welcome, in wake-ups, in QR codes and in the
self-signed certificate. The SIP domain, which is the part after the @ in a phone's address such as
201@site, defaults to that host. Because the domain is baked into every phone's stored credential,
it should be chosen once and not changed.

Both server programs are started with command-line options. There is no configuration file; a change
of options means a restart. Per-phone data, the licence and the log level can be changed while running.

## 6. How calls work, step by step

The server is what telephony people call a back-to-back user agent. It does not pass a call through;
it terminates each side as a separate call leg and joins the two. This is forced by iOS: a suspended
app cannot hold a registration, so a PBX would have nowhere to send the call. The server is the
always-on endpoint that answers for the phone, wakes it, waits for it, and then bridges. A side effect
is that the server can insist on encryption towards the phone, carry the audio, play hold music and
handle transfers itself, whatever the PBX can do.

### Rules that apply to every call

- The caller hears ringing only when something is actually ringing: the called phone has started
  alerting, or a wake-up has reached a device. An unreachable destination fails at once with no
  ringing, and the caller's app plays a congestion tone straight away.
- The app plays its own call-progress tones at the European standard 425 Hz: ringback (1 s on, 4 s
  off), busy (half-second on and off, for 4 s), congestion (quarter-second on and off, for 3 s) and a
  call-waiting beep (two short beeps every 5 s). A failure tone keeps the call on screen for its own
  length.
- What the caller is told on failure: "busy" and "declined" are relayed as they happened; every other
  failure, including no answer and unknown number, is reported as "unavailable".

### Incoming call from the PBX to a phone (trunk mode)

1. The PBX sends the call to the server's trunk listener.
2. The server looks up the destination. It is one of its own phones.
3. If the phone is registered and its connection is alive, the server sends the call to it directly
   and sends a wake-up as well. If not, it sends a wake-up on every live connection the phone has.
4. If the phone has no live connection at all, the caller gets "unavailable" immediately.
5. The extension or the app reports the call to iOS and tells the server "will answer". The server
   tells the PBX the phone is ringing.
6. The user answers. The app registers to the server over SIP, the server places the call to the app,
   and bridges the two legs, with audio relayed through itself.
7. The name shown on the phone is, in order of preference: the matching directory entry, the caller's
   own display name as sent by the PBX, or the bare number.

The caller rings for the ring timeout, 30 seconds by default. That is also how long a woken phone has
to register. If it has not by then, the wake-up is cancelled and the caller gets "unavailable".

### Outgoing call from a phone to the PBX

1. The user dials from the keypad, the directory or recents. The app sends the call to the server,
   authenticated with the phone's credential.
2. A number that is not one of the server's own phones goes to the PBX.
3. The server offers the PBX the configured codecs (by default G.722, then G.711 µ-law and A-law),
   narrowed to what the phone can do.
4. In trunk mode the caller identity shown to the PBX is the phone's extension at the server's domain.
   In lines mode it is the phone's line at the PBX (section 8).
5. Over a TCP or TLS trunk, if the PBX gives no response at all within 3 seconds the server assumes the
   connection is dead, redials once on a fresh connection, and reports "unavailable" if that fails too.

### Phone to phone with no PBX

A phone calling another phone's extension is routed within the server. The PBX never sees these calls
and they keep working during a PBX outage. With no PBX configured at all the server is self-contained.
Both legs are encrypted and the Opus codec is preferred.

### Hold

Hold is a standard SIP re-negotiation between the app and the server. The server then plays hold music
to the other party on that party's existing audio stream, without telling the far end anything. Because
nothing is signalled, this works with any PBX, carrier or desk phone. The music is built into the
server (three clips: music, ringback and busy, pre-encoded for each codec); changing it needs a new
build. When the PBX side puts a call on hold, the PBX plays its own music.

### Transfer

Transfer is blind: the user enters a number, the caller is connected to it, and the user's call ends.
The server handles the transfer itself; the far end sees nothing unusual.

- The target is treated like a fresh call: another phone (woken if needed), the PBX, or the echo test.
- The waiting party hears music while the target is found and ringback once it starts ringing.
- The person transferring is released the moment the target starts ringing.
- If the target refuses before ringing, the original call carries on and the phone shows "Transfer
  Failed" with the reason for 6 seconds. If it fails after ringing, the waiting party hears busy and
  the call ends.
- When both the remaining party and the target are on the PBX, the server asks the PBX to complete the
  transfer and drops out of the audio path. It waits up to 10 seconds for the PBX and completes the
  transfer itself if the PBX refuses.
- If the new party uses a different codec, the remaining leg is re-negotiated to match.

Attended (announced) transfer is not built.

### Call waiting and a second call

A phone can have two calls at once: one active and one on hold. Answering a second call puts the first
on hold; on iOS 26 this is the system's plain Accept, on earlier iOS it is "Hold & Accept". The user can
swap between them. When the active call ends, the held one comes back automatically. A third caller,
or any second caller while Call Waiting is switched off in the app's settings (it is on by default),
hears busy and the call is recorded as missed. A second outgoing call during a call is not built.

### Declining

When the user declines a ringing call, the server immediately tells the caller "busy", so the PBX
applies its busy or forward-on-busy rule. There is one exception: when iOS itself silences the call
because of a Focus or Do Not Disturb setting, the server treats it as unanswered, the caller keeps
hearing ringing until the 30-second timeout, and then gets "unavailable".

### Caller hangs up while the phone is ringing

The server stops the call to the phone at once and sends a cancellation. If the phone is between
connections at that moment, the cancellation is replayed when it next connects, so the phone does not
ring for a caller who has gone.

### Failures

- An unknown number with no PBX: "unknown number" to the PBX, "unavailable" tone on a phone.
- A phone that is not registered and cannot be woken: "unavailable" at once.
- A phone that was woken but did not register within 30 seconds: "unavailable".
- A phone whose connection dies while it is ringing: the caller rings to the timeout, then
  "unavailable". This is deliberate; a brief network blip should not kill a ringing call.
- PBX extensions: whether a desk phone is registered is the PBX's business. The PBX returns its own
  busy or unavailable result and the server relays it.

### A party that vanishes mid-call

Each leg of a connected call is probed every 15 seconds with a SIP liveness request. A party that has
answered nothing for 60 seconds (the peer timeout, configurable) ends the call. This is the backstop
for a phone that crashed, left Wi-Fi range or was suspended without hanging up. The phone's own engine
also hangs up after 30 seconds with no audio arriving.

### Echo test

Dialling the word "echo" is answered by the server itself, which plays the caller's own voice back
through the relay after a short delay. It tests the whole path from microphone to server to speaker.
Because the keypad types digits only, "echo" is normally reached from a directory entry. A PBX may
offer its own echo extension (600 in the test set-up), which tests the PBX path as well.

## 7. Audio and encryption between phone and server

The connection between a phone and the server is held to a strict profile. The phone registers only to
the Dialpark server, never to a PBX, and never holds a PBX credential.

**Signalling.** SIP over TLS only, TLS 1.3, on port 5061, using the same server certificate as
everything else. Every registration and every new call is challenged with SIP digest authentication:
the username is the device identifier and the password is the credential issued at enrolment. One
phone cannot register or call as another. Registrations last 300 seconds and are refreshed by the app.

**Reaching the phone.** The server records each registration as the address the phone actually
connected from, over the TLS connection it already has, and places calls to the phone down that same
connection. Phones behind network address translation or firewalls are therefore reachable with no
special configuration. If that connection has gone, the phone counts as unregistered and is woken
instead.

**Audio.** All audio between phones and the server is encrypted with SRTP, keyed through the encrypted
signalling. There is no unencrypted option on the phone side. Audio always flows through the server;
there is never a direct phone-to-phone path. The relay copies the encoded audio without re-encoding
it, so both legs of a call always use the same codec and nothing is lost in translation. The relay
sends audio back to wherever the phone's own audio came from, which is what makes it work through
address translation.

**Codecs.** Phone to phone: Opus, at 32 kbit/s mono with forward error correction. Phone to PBX: G.722
(wideband) preferred, G.711 as the fallback. The phone offers Opus, G.722 and G.711 µ-law; the server
also offers A-law towards a PBX. There is no G.729. Touch tones are carried as standard SIP telephone
events on the network, though the app has no in-call keypad to send them.

**Quality under loss.** Opus uses in-band error correction; for G.711 and G.722 the app has its own
packet-loss concealment. Jitter buffers adapt between 40 and 160 milliseconds. Under a test impairment
of 2% packet loss and 30 ms of jitter, a call shows at most one or two dips of 20 to 40 milliseconds.

**Network priority.** Audio packets are marked with the standard voice priority (DSCP EF) and
signalling with the signalling priority (CS3), by the app, the server and the PBX configuration, so
network equipment that honours those marks gives calls precedence. The app also tells iOS the
connection is voice, so Wi-Fi uses its voice access category.

**Measured delay.** The automated audio gate requires mouth-to-ear delay of at most 150 ms on a call
echoed by the server and 200 ms on a call echoed through the PBX, with no gaps. Recent runs measure
80 to 120 ms.

## 8. Connecting to a PBX

The PBX connection is chosen when the server starts: either a **trunk** or **lines**. Never both,
because an exchange matches incoming SIP by source address before anything else, and a trunk would
swallow the calls of registered lines. With no PBX configured the server is standalone.

### Trunk mode

The PBX treats the server as a trusted peer, identified by its network address, exactly as it would
treat another exchange or a carrier.

- Transport is UDP, TCP or TLS. The server listens for calls from the PBX on port 5060, or 5062 for TLS.
- Calls from the PBX are accepted without a password challenge. On a TLS trunk the source must be the
  configured PBX address or one of a list of additional peers, which covers PBX clusters where any node
  may originate a call.
- TLS can be mutual: the server presents its own certificate to the PBX and verifies the PBX against a
  private certificate authority, the shape Cisco CUCM uses for secure trunks. The minimum TLS version
  towards the PBX defaults to 1.2, because secure CUCM trunks generally speak 1.2; the phone side stays
  at 1.3.
- Audio towards the PBX is unencrypted by default. It can be switched to SRTP, in which case the server
  offers encrypted audio and refuses any call that does not end up encrypted. There is deliberately no
  "encrypt if possible" mode. The server warns if encrypted audio is used over an unencrypted trunk,
  because the audio keys travel in the signalling.
- On a TCP or TLS trunk the server probes the PBX every 10 seconds. A probe unanswered for 3 seconds
  drops the pooled connection so that the next call dials afresh instead of hitting a dead socket. This
  probe does not run over UDP, which has no connection to go dead; the console then shows the trunk's
  probe state as "off". The server also answers the PBX's own probes, which CUCM requires to keep a
  trunk marked up.
- Codecs offered to the PBX are configurable; the default is G.722 then G.711. The server never
  transcodes, so the phone gets whatever the PBX chose. Incoming calls follow the PBX's offer order.
- Any number that is not one of the server's own phones is sent to the trunk, including numbers that
  came from the trunk. The PBX should therefore route only the provisioned extensions to the server,
  not a whole range, to avoid loops.
- For CUCM: enable Early Offer on the trunk (the server refuses invitations without media details),
  allow G.722 and G.711 in the region, and for TLS use an encrypted security profile with the server's
  certificate name and its CA uploaded as trusted.

### Lines mode

The server registers to the PBX on behalf of each phone, as if each phone were an ordinary third-party
SIP desk phone. The app is unchanged and still talks only to the server; it never sees a PBX credential.

- Each phone gets a PBX digest username and secret, entered in the console. The secret is write-only
  and stored encrypted. On CUCM each line is a "Third-party SIP Device" with its own end user and
  digest credentials, and consumes a CUCM licence; Cisco requires a distinct user per device.
- The server keeps every line registered around the clock, refreshing at three quarters of the lifetime
  the PBX grants (one hour requested by default). Temporary failures are retried with increasing delays
  up to five minutes. A rejected credential makes the line "refused": the server logs once and stops
  until the credential is changed, so a wrong password never becomes a retry storm.
- Line states shown in the console: pending, registered, retrying, refused.
- Lines are unregistered when a phone is revoked, when it loses its licence seat, and on a clean
  shutdown.
- Incoming calls arrive at the registered line and are delivered to the phone as usual. Outgoing calls
  identify as that line at the PBX domain and answer the PBX's authentication challenge.
- Because a line is always registered, a PBX "forward when unregistered" rule never fires; "forward on
  no answer" works.
- The line connection is plain SIP and RTP, which is the documented CUCM profile for this device type.
  Encrypting it is not scheduled.

### Which PBXs

Asterisk is the development and test PBX, in both modes and all transports. Cisco CUCM is supported by
design, with a bench checklist waiting for a live system; it has not yet been tested against one. Any
standard SIP exchange that can define a trunk by address, or register a third-party SIP phone, should
work.

## 9. Setting up a phone: enrolment

A phone is added in the console, which produces a single-use code, and the code is entered on the
phone. Nothing is typed into the phone except, at most, the server address and the code.

### In the console

The administrator enters the phone's extension and a short description ("Warehouse 3"). The server
creates a record and an enrolment code: 8 characters, valid for 15 minutes, usable once, stored only as
a hash. The console shows the code as text and as a QR code. The QR holds a link containing the
server's address, the port, the code and a fingerprint of the server's certificate. A new code can be
issued at any time; issuing one cancels the previous one.

### On the phone

The first screen shows the Dialler mark, the heading "Dialler" with "for Dialpark" beneath it, and two
buttons: Scan QR Code and Enter Code Manually.

- **Scan.** The in-app scanner reads the QR. Alternatively the iOS Camera app recognises the link and
  opens Dialler with it. The certificate fingerprint in the QR means the phone trusts exactly the right
  server from the very first connection.
- **Type.** Server address, port (8080 by default) and the code. Mistypes are forgiven: case does not
  matter, dashes are ignored, and I or L are read as 1 and O as 0. With no fingerprint available the
  phone trusts the certificate it sees on this first connection, then checks it against the one the
  server's reply names; if they differ it refuses with "The server's certificate does not match its
  enrolment reply. Do not continue on this network."

The phone presents the code. The server checks it (constant-time, with a half-second delay on a miss,
and at most 5 attempts a minute per address) and, if there is a free licence seat, replies with the
device identifier, the extension, a fresh credential, the signalling port, the SIP domain and the
certificate fingerprint. Claiming a code rotates the credential: a phone that previously held this
device's credential stops working. If no seat is free the phone is told "The server has no free licence
seats. Ask your administrator." and the code remains valid for a later attempt.

### What the phone stores

The server address and port, the device identifier and the certificate fingerprint are kept in the
app's shared settings; the credential is kept in the iOS Keychain. The directory, recents, logs and
pending diagnostics are kept in the app's shared storage so the extension can read what it needs.

### Permissions

The app asks for the camera only when the scanner opens ("Dialler uses the camera to scan the
enrolment code your administrator shows you"), for local network access during set-up (it opens a test
connection and waits for the answer, so the first attempt does not fail), and for the microphone once
the phone is set up. If local network access is refused the app says so and offers an Open Settings
button.

### Certificate pinning

From enrolment on, every connection the phone makes to the server, for signalling, SIP and HTTPS, must
present the exact certificate whose fingerprint was recorded. A self-signed certificate works
perfectly well for this. The consequence is that replacing the server's certificate means re-enrolling
every phone, which is why the advice is to install a long-lived certificate before the first phone is
enrolled.

### Replacing, revoking, signing out

- **Replace a phone:** issue a new code for the same device. The new phone takes over the extension,
  its directory and its settings; the old phone stops working.
- **Revoke:** the phone is disconnected and can no longer register or connect. Its record, directory,
  settings and PBX line are kept, so enrolling it again restores everything. A call in progress runs to
  its end.
- **Purge:** deletes the record, its directory and its diagnostics, and frees the extension at once.
- **Sign out on the phone:** removes the credential, the background-call configuration, the directory
  copy and the extension number. The server record is untouched. Recents are kept.

## 10. The Dialler app, screen by screen

Requirements: iPhone, iOS 17 or later, portrait only. The current App Store version is 1.1.1.

### Tabs

**Recents** (the opening tab; a badge counts unseen missed calls). A list of calls with a filter for
All or Missed. Each row shows the name (looked up in the directory each time it is drawn), the
extension or number, the outcome or duration, a direction arrow or a cross for missed, and the time.
Tapping a row calls back. Calls are recorded on the phone only; the server keeps no history. The newest
500 are kept with no time limit. Calls that arrived while the app was closed are recorded too: the
extension leaves a note for each wake-up it handles, and the app folds those in. Outcomes include
completed, missed, declined, cancelled, answered elsewhere, busy, no answer, unavailable and failed.

**Keypad.** Shows "Calling from Ext 204". Digits, star and hash, with a long press on 0 for plus. The
call button with nothing typed redials the last number. After a completed call the number clears; after
a failure it is kept for correction or redial. Calling is disabled during a call.

**Directory.** The phone's own address book, with a search field that matches any part of a name or
number ignoring case and accents, a Favourites section, then alphabetical sections. Swipe right to
favourite, swipe left to edit or delete, long press for Call, Favourite, Edit and Delete. Adding or
editing a contact asks for a name, a number or SIP address, a route (Direct, meaning another Dialler
phone on this server, or Phone System, meaning via the PBX) and a favourite flag. The route is a label;
the server decides the routing. Edits are written to the server first and need the phone to be online;
if the write fails nothing changes and an alert says why. See section 12 for how directories work.

**Settings.** At the top, the extension ("Ext 204") and the connection state: Connected, Connecting,
Waiting for Network, Not Connected, Demo, or a refusal message. Then:

- Connection: the server address; Background Calls as On, Waiting for Wi-Fi (configured but not on a
  listed network) or Off; and the assigned Wi-Fi names, read-only. A footer explains what each state
  means for receiving calls.
- Calls: a Call Waiting switch, on by default. "When this is on, a second caller rings while you're on
  a call. When it's off, they hear a busy tone."
- About: the version, and Acknowledgements listing the open-source components with their licence
  texts.
- Sign Out, with the confirmation "Calls won't reach this iPhone until it's set up again with a new
  enrolment code from your administrator."

### Receiving a call

The phone rings through the standard iOS call interface: the full-screen call on a locked phone with
slide to answer, or the banner when unlocked. Calls also appear in the iOS Phone app's recents. The
displayed name follows the order given in section 6. If a better name arrives while the phone is still
ringing, the banner updates. Ringing lasts for the server's ring timeout, 30 seconds by default; the
wake-up carries the deadline and the app corrects for a phone clock that differs from the server's by
more than 5 seconds.

### In a call

A full-screen view with the other party's initials, name and a status line (Calling, Ringing,
Connecting, a running timer, On Hold, or a failure such as Busy, Declined or Unavailable). Buttons:
Mute, Hold (which becomes Swap when a second call is held, on iOS 17 and 18; iOS 26 provides its own
swap banner), Speaker, Transfer (enabled once connected) and End Call. Bluetooth and CarPlay routing
are handled by iOS. There is no in-call keypad, no video and no conference.

### Licence messages

In the last seven days of the licence the app shows a yellow banner above every tab: "This server's
licence expires in N days. Ask your administrator to renew it." (or "tomorrow", or "today"). If the
server refuses the phone, the Settings summary shows why: no free seat, licence expired, no licence or
needs updating, or licence could not be verified. Any other refusal, such as a revoked device, shows
"Not Accepted by the Server". After a refusal the app stops retrying until it is next opened.

### Demo mode

Entering the code DEMODEMO with no server address puts the app into a self-contained demonstration.
No server is involved and nothing leaves the phone. The phone becomes Ext 200 with a ten-entry
directory. Outgoing calls ring, connect and play the user's voice back; calls to "Sales Line" are
always busy. A Settings button, "Receive a Demo Call", makes "Reception" ring through the normal iOS
call screen a few seconds later. Sign Out leaves the demo. This is what Apple's reviewers use.

### Diagnostics from the phone

At every launch the app uploads its own log and the extension's log, plus any iOS crash, hang or
excessive-CPU reports, to the company's server only. Nothing is ever sent to the vendor. Section 13
describes what is in them.

### The hidden Status page

Developer builds have a diagnostic page opened by pressing the Settings title for five seconds. It
shows raw connection and engine states, allows a manual connection, lets a developer type Wi-Fi names
by hand or remove a stale background-call configuration, sends diagnostics on demand and shows the
live log. It is absent from App Store builds.

## 11. The admin console, page by page

The console is a website served by dialpark-admin, normally at https://127.0.0.1:8443 on the server
machine, or on a wider address if started that way. The browser warns once about its self-signed
certificate unless a real one is supplied. There is one account, username admin, with a password read
from a file at start. A session lasts 12 hours from last use; restarting the console signs everyone
out. A form submitted after the session expired is kept for 15 minutes and applied after signing in
again. Every form is protected against cross-site forgery. Shift-D switches between light and dark.

If the call server is unreachable, every page shows a banner, "the call server is not answering.
Nothing can be changed until it does. Showing what was last read at …", and every control that writes
is disabled. Nothing the console does can interrupt a call.

The header has five pages: Server, Clients, Calls, Diagnostics, Licence. "Client" is the console's
word for one phone.

### Server

Read-only. Identity: version, up since, mode (trunk or lines), public host and SIP domain, the listening
ports, the certificate with its expiry and fingerprint and the warning "Every phone pins this
certificate; replacing it means re-enrolling every phone", the ring and peer timeouts, the audio port
range, the data directory and the diagnostics retention. Counts: clients (enrolled and revoked), online
apps and wake extensions, SIP registrations, PBX lines registered and failing, calls in progress, the
licence summary, and admin API counters.

If a trunk is configured, a Trunk section shows the PBX address, the probe state (up, down, or off for a
UDP trunk, with the time since and the last error), audio encryption and codecs, and the TLS settings
including warnings when the PBX certificate is not verified or no certificate is set. In lines mode a
Lines section shows the registrar, domain, registration lifetime and default line.

### Clients

A line of totals (clients, online, registered, on a call, revoked, seats used of seats available,
clients without a seat, trunk state), an Add a client button, and a table sorted by extension with
columns Ext, Client, Phone, SIP, PBX line and State. Each column has a help bubble. Phone shows "App
1.4" when the app is connected (the number is the version it reported), "Wake only" when only the
extension is connected, or "Offline". SIP shows Registered or a dash. PBX line shows registered,
pending, retrying, refused, stored (a line is configured but the server is in trunk mode or the client
is revoked) or a dash. State shows Enrolled, Not enrolled, On a call with its duration, or Revoked, plus
"code valid until" when a code is outstanding and a red "No seat" chip when the licence has none for
it.

**Add a client** asks for the extension and a description, then shows the enrolment page: a QR code,
the typed code in large letters, the three steps for the phone ("Install Dialler on the phone and open
it", point the Camera at the QR or type the server and code, the phone connects and registers), the
validity time, and buttons to make a new code, cancel the pending code or finish. A code is shown once;
leaving the page means making a new one.

**The client page** for one phone has:

- Status: app connection (when, from where, which version), wake extension connection, SIP
  registration and its expiry, PBX line state with realm, error and attempts, and the current call.
- Enrolment: when the credential was issued, whether a code is pending, and buttons to make a code,
  re-enrol with a new code (which rotates the credential, so the current phone stops working until it
  re-enrols) or cancel the pending code.
- Settings: the description; the Wi-Fi networks for background wake-ups, one per line, with "Save and
  push" (an empty list stops background wake-ups); and the PBX line (digest user, write-only secret,
  and the directory number, which defaults to the extension).
- Directory: version and contact count, Download CSV, Upload CSV, in-place editing of each contact
  (favourite, name, number or address, via this server or the PBX), an add row, and "Copy this
  directory to other clients", which replaces each chosen phone's directory with this one and reports
  per phone.
- Diagnostic files: everything this phone has uploaded, with download and delete.
- Remove: Revoke, with a confirmation, and Purge, which requires typing the client identifier.

### Calls

Read-only, refreshed on load; the server rebuilds the view at most once a second. For each call in
progress: when it started and for how long, its state (ringing, waiting for wake, bridged, held and by
whom, or being handed over in a transfer), both parties with name, address, leg type and whether the
leg is encrypted, the codec on each leg, and relay counters in each direction (packets, lost, jitter).
There is deliberately no hang-up button.

### Diagnostics

Server logging: the current level and whether SIP tracing is on, and a form to raise the level or turn
on tracing for a limited time (at most an hour), after which it reverts on its own. Files uploaded by
phones: a table per client with counts and the latest upload time. Recent events: the latest 100
server events, such as a phone coming online, a registration, a line or trunk state change, a call
starting or ending, a code being issued or claimed, an admin change, or a licence seat being lost or
gained.

### Licence

The installed licence: its state (active, expiring, missing, invalid, wrong install, expired), who it is
licensed to, seats in use of seats available, the validity date and days left, and the install id with
the note "Give this to your vendor; a licence is issued for one install id and works on no other
server." A text box to paste a new licence and an Install button. The explanatory text changes with
the state, for example for an expired licence: "every client is suspended until a new one is
installed below."

## 12. Directories

Each phone has its own directory, held on the server, which is the single source of truth. There is
no shared or global list. The administrator and the phone's user edit the same list, favourites
included.

- A contact has a display name, a number or SIP address (a bare number becomes an address at the
  server's domain), a route (this server or the PBX), and a favourite flag. Duplicates are merged by
  address. A directory holds up to 5,000 contacts.
- Every change raises the directory's version. The server tells the phone when its directory changed,
  and the phone fetches only what changed, deletions included. The phone also syncs when it connects,
  after each of its own edits, and on pull to refresh. The list is kept on the phone so it shows before
  the first sync and while offline, though edits need the server.
- In the console a directory can be edited row by row, downloaded as CSV, replaced from a CSV upload
  (after a preview that says how many contacts would be added, changed and removed, and refused if
  someone else changed the directory in between), or copied to other phones.
- The CSV has a header row and the columns display_name, uri, mode (local or trunk) and favourite, in
  any order, UTF-8, up to 4 MiB. Errors name the offending line.

## 13. Diagnostics and logs

**From phones.** At every launch, and in developer builds on demand, the app uploads its log and the
extension's log (each rotating at 2 MB, cleared once queued) and any iOS crash, hang or CPU reports, to
the company's server over the pinned connection. The server stores them per device, named by time and
kind, keeps them for 30 days by default (configurable, or forever), and lists them in the console for
download or deletion. Logs have the SIP password removed but otherwise contain device identifiers,
the server address, Wi-Fi names, numbers and names of callers and callees, and call identifiers. They
are not anonymised, and they go only to the company's own server.

**On the server.** The server logs to its standard error stream, as text or JSON, at a chosen level.
Debug level and full SIP tracing cost processing time on the call path, so when switched on at runtime
they are given a duration and revert on their own; they are never persisted across a restart. The log
records call activity (which is why the privacy policy says the server's operating logs record calls
even though it keeps no call history).

**Events.** The server keeps the last 4,096 events in memory, visible in the console and the admin
API. They record what the server did, not who asked; there is no audit trail of operator identity,
because there is a single operator account.

## 14. The licence

A licence allows a number of phones (seats) until a date, on one server installation.

**The token.** A licence is a text token of the form DP1, a dot, a readable payload, a dot, and a
signature. The payload states the licence identifier, the customer name, the server's install id, the
number of seats, the issue date and the validity date. It is readable, not secret; what makes it a
licence is the vendor's Ed25519 signature, which covers the prefix and the payload. Changing a single
character breaks it. The vendor's public key is compiled into both the server and the app; the private
key never leaves the vendor and the tool that uses it works offline.

**The install id.** At its first start the server creates a 32-character install id and stores it in
its data directory. The id is never rewritten. The console shows it; the customer gives it to the
vendor; the vendor issues a licence for that id, which no other server can use. If the install id file
is lost, the licence stops matching, which is why it is backed up with the rest of the data.

**Installing.** The licence is pasted on the console's Licence page and takes effect at once. The server
refuses a token that is not a licence, has a bad signature, names another install id, has expired or
says something it does not understand, and says which.

**States.** Missing, invalid, wrong install, active, expiring (inside the last 30 days) and expired.
Seats count only when active or expiring; in every other state there are zero seats, so no licence
means no phones can connect. The server checks the clock every minute, warns daily in the last 30 days,
and the app shows its banner in the last 7.

**Seats.** A phone that is enrolled and not revoked holds a seat. Creating a phone's record and its
enrolment code is always allowed, so phones can be prepared before a licence arrives; the seat is
taken when the code is claimed. If there are more holders than seats, because a smaller licence was
installed or the licence expired, the phones that have held a seat longest keep theirs and the newest
are suspended, not deleted. Revoking a phone frees its seat.

**What suspension means.** A suspended phone is refused at every door: the wake-up connection tells it
why ("no licence seat", "licence expired" or "no licence"), SIP registration is refused, the directory
and diagnostics routes are refused, an incoming call to it fails as unavailable, and in lines mode its
PBX line is unregistered so the exchange sees it offline. A call already in progress is never cut.
Installing a larger or renewed licence restores the phones immediately.

**The phone checks too.** The welcome carries the licence token, and the app and the extension verify
the signature and the validity date themselves, against the phone's own clock. A customer can alter a
server they control, or set its clock back, but cannot alter the App Store app, so authenticity and
expiry are enforced on the phone while seat counting is enforced on the server. A phone whose clock is
badly wrong may refuse a valid licence. Accepted residual risks: a modified server could over-enrol,
and a copied data directory could run a second server.

**The vendor's tool.** It creates the key pair once (and refuses to overwrite it, because a second key
would strand every licence issued under the first), issues a licence for a customer, install id, seat
count and last valid day (expiry is the start of the following day, UTC), and inspects any token. A
renewal is a new licence for the same install id.

## 15. Data, backup, certificates, restarts

**The data directory** (by default a folder named data next to the server) holds: the device records
with their credential hashes, codes, settings and encrypted PBX line secrets; one directory file per
phone; the install id; the licence; the key that seals the line secrets; the self-signed certificate
and its key; the diagnostics uploads per phone. The console keeps its own password and certificate in
its own folder.

**Backup** is a plain copy of that directory, which can be taken at any time because every file is
written atomically. These must stay together: the device records, the sealing key (without it no line
secret can be read), the certificate folder (without it every phone must re-enrol), and the install id
with the licence. Restore is stop, replace the directory, start.

**Certificates.** With none supplied, the server creates a self-signed certificate valid for one year
and keeps it, so phones' pins survive restarts. A supplied certificate is used as is. Either way, every
phone pins whatever it saw at enrolment, so rotating the certificate means re-enrolling every phone.
Install a long-lived certificate before the first phone enrols.

**Restarts.** A server restart drops every call in progress, because audio flows through the server,
and forgets registrations, pending wake-ups, the event list and any runtime log level. Device records,
credentials, directories, settings, the licence and the certificate are all kept. Phones reconnect by
themselves within seconds and re-register; nothing needs re-enrolling. Restarting the console affects
nothing but signed-in sessions.

**Running as a service.** The product ships as two programs started with options. Installing them as
an operating-system service (so they start at boot and restart on failure) is left to the installer;
no service definition is included.

## 16. Security model

**Encrypted everywhere on the phone side.** TLS 1.3 for the wake-up channel, for SIP and for HTTPS;
SRTP for all audio. There is no unencrypted mode towards phones. Towards the PBX, signalling and audio
are plain by default and can be TLS (with mutual certificates) and SRTP.

**Pinned.** Phones accept only the certificate they saw at enrolment, so nobody on the network can
impersonate the server to an enrolled phone. The console verifies the server's certificate too.

**Authenticated.** Every registration and call from a phone is challenged with a credential issued at
enrolment and bound to that device and extension; one phone cannot act as another. Enrolment codes
have 32 to the power of 8 possible values, last 15 minutes, work once, are compared in constant time
with a half-second delay on a miss, and are limited to 5 attempts a minute per address.

**Secrets and where they live.** The admin token (a file shared by both server programs), the operator
password (a file), per-device credentials (stored only as hashes), PBX line secrets (encrypted at rest
with a key in the data directory), the trunk TLS private key, and the vendor's licence signing key
(with the vendor only). No API returns a secret and none is written to the logs.

**The admin API** listens on the loopback address only unless deliberately widened, requires the
bearer token, rejects unknown or oversized input, limits connections and request rates, and is proven
under a 60-second flood (thousands of requests a second, large uploads, hundreds of idle connections,
malformed bodies) to leave a call's audio and a third phone's registration and dialling times
untouched.

**What someone on the network can still do.** Claim an enrolment code if they see the QR before the
phone does and within its 15 minutes, in which case the administrator sees the device come online from
an unexpected address. Reuse an authentication nonce within its 5-minute life. Read SRTP keys if trunk
encryption is run over an unencrypted trunk, which the server warns about. With a UDP or TCP trunk, the
trunk listener accepts calls from any address without a challenge, so it should be reachable only from
the PBX; firewall it accordingly.

## 17. How the product is tested

Almost the entire system can be exercised with no phone in hand, because the foreground app and the
background extension speak the same messages to the same server. An automated test bench runs in
containers: the real server, an Asterisk PBX with a desk phone and an echo extension, a SIP conformance
tool, headless software phones that play and record audio, and a headless stand-in for the app's
wake-up path.

What it proves on every run of the full regression:

- SIP conformance: registration over TLS, plain TCP refused, correct results for offline and unknown
  destinations, unregistration.
- A real phone-to-phone call and the wake-up path, asserted on recorded audio.
- Audio quality: delay at most 150 ms on a server echo and 200 ms through the PBX, with zero gaps; a
  separate run under 2% packet loss and jitter checks concealment.
- Network priority marks on every audio and signalling packet.
- The PBX trunk in all its variants: both directions, transfers, encrypted audio, mutual TLS, both
  together, narrowband codecs, a stalled trunk connection, the PBX's own hold and resume, an
  unavailable PBX extension, hold music, a caller hanging up before answer, a party vanishing, a dead
  callee connection.
- Lines mode: registration, calls in and out, and a wrong password latching as refused.
- The licence: install, a refused paste, a shrink that suspends exactly the newest phones, restore.
- The admin API under flood while a call is up, and the console end to end.
- On the iPhone simulator, the real app engine against the real server: incoming, outgoing, two calls
  in a row, decline, a reset during ringing, a refused call with its tone, a server restart under a
  registered phone, and call waiting.

What still needs a human and a phone: the extension starting when the phone joins a listed Wi-Fi and
surviving the app being killed; a call ringing from the extension and cold-launching the app; real
audio routes (earpiece, speaker, Bluetooth, CarPlay, a cellular call interrupting); Apple's own
conformance checks for the push transport; call waiting with the phone locked and unlocked; tone
levels; the three enrolment methods and the certificate refusal; recents after missed, answered and
declined calls; and the CUCM checklist once a CUCM is available.

## 18. Limits and things that are not built

Scope and platform

- iPhone only, iOS 17 or later.
- Background calls only on the assigned Wi-Fi networks. No cellular or off-site use, no remote
  workers, no Apple push fallback.
- One server per phone; one phone per extension.
- No video, no messaging, no conference.

Calls

- Blind transfer only; no attended transfer.
- No second outgoing call during a call; at most two calls per phone.
- No in-call keypad, so no touch tones can be sent during a call.
- No ringtone choice, voicemail, forwarding, presence or Do Not Disturb settings in the app; a Focus
  mode on the phone makes the caller ring to the timeout rather than hear busy.
- A call that spans a move between two Wi-Fi networks keeps its audio on the old address until it
  drops.

PBX

- No G.729. No transcoding. No "encrypt if possible" on the trunk.
- Delayed-offer invitations (without media details) from a PBX are refused; CUCM must use Early Offer.
- Cisco CUCM is untested on a live system.
- The PBX should route only provisioned extensions to the server; there is no owned-range setting yet.
- The lines connection is unencrypted.

Operations

- Replacing the server certificate strands every phone until it re-enrols.
- No configuration file; options are command-line flags and need a restart.
- No service definition is shipped.
- Hold music is built in; changing it needs a new build.
- The server keeps no call history and no audit trail of operator actions.
- Designed for up to 500 phones and 5,000 contacts per directory; the console does not paginate.
- The version the app reports to the server is currently a fixed placeholder, so the console's "App
  1.4" style figure does not yet reflect the real app version.

## 19. Third-party components

All components are under permissive licences that allow them to be built into a closed-source,
commercially sold product. Their notices are reproduced in the app's Acknowledgements screen.

| Component | What it does | Licence |
|---|---|---|
| baresip and libre 3.15 | The SIP and media engine in the app | BSD 3-Clause |
| Opus 1.5 | The phone-to-phone audio codec | BSD 3-Clause, royalty-free |
| OpenSSL 3.3 | TLS and SRTP cryptography in the app | Apache 2.0 |
| G.722 | Wideband codec for PBX calls, from a public-domain implementation | Public domain |
| G.711 | Narrowband codec, from baresip | Royalty-free |
| Packet-loss concealment | Written for this product from the ITU-T G.711 Appendix I description | Own code |
| sipgo and diago | SIP and media libraries in the server | BSD 2-Clause and MPL 2.0 |
| Go | The server language and runtime | BSD-style |

Components deliberately avoided for licensing reasons: PJSIP and Linphone (GPL) on the phone, and
Kamailio, rtpengine or an embedded Asterisk (GPL) on the server. Asterisk is used only as a test PBX
and is not distributed.

## 20. Glossary

- **App push provider, push provider extension**: the part of the Dialler app that iOS runs in the
  background on assigned Wi-Fi networks to receive wake-ups.
- **B2BUA (back-to-back user agent)**: a server that terminates a call on each side and joins the two,
  rather than passing one call through. The Dialpark server is one.
- **CallKit**: Apple's framework that shows incoming and active calls in the standard phone interface.
- **Codec**: the method used to compress voice. Opus, G.722 and G.711 are used here.
- **Digest authentication**: the SIP password challenge used for registrations and calls.
- **DSCP**: a mark on network packets that routers and Wi-Fi use to prioritise voice.
- **Enrolment code**: the 8-character single-use code that connects a phone to a server.
- **Install id**: the server installation's permanent identifier, to which a licence is bound.
- **Lines mode**: the server registers one SIP line per phone to the PBX.
- **Local Push Connectivity**: Apple's mechanism for waking an app from a local server on designated
  Wi-Fi networks, without Apple's push service.
- **PBX**: the company's phone exchange, such as Asterisk or Cisco CUCM.
- **Registrar, registration**: the server-side record that says where a phone can currently be reached;
  the phone refreshes it every five minutes.
- **Relay**: the server component that carries audio between the two legs of a call.
- **Seat**: one phone's allowance under the licence.
- **SIP**: Session Initiation Protocol, the standard signalling language of IP telephony.
- **SRTP**: encrypted voice packets.
- **SSID**: a Wi-Fi network's name.
- **Trunk mode**: the server and the PBX trust each other by network address, as two exchanges do.
- **Wake, wake-up**: the server's message telling a phone a call is arriving, delivered over port 7443.
