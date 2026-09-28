# App Store submission plan

The work needed to take the iOS app from its current state (branch-built,
installed from Xcode) to a public App Store release. Written 2026-09-28
from a review of the project as it stood on `main` at 58bb0c7.

## Decisions

| # | Question | Decision |
|---|----------|----------|
| 1 | Distribution route | **Public App Store** (2026-09-28). |
| 2 | How App Review uses the app | **Open.** A public demo server, or a demo mode in the app. Compared under [Review access](#review-access); recommendation: demo mode. |
| 3 | The hidden Status page | **Open.** It is a hidden feature (guideline 2.3.1). Release builds must at least drop its developer fields; keep a documented diagnostics page, or remove the page from release builds. |
| 4 | Store name | **Done.** "Dialler" is reserved; the app record exists in App Store Connect (pre-submission). |

## Progress

On `feature/app-store-readiness` (2026-09-28), awaiting approval:

| Item | State |
|------|-------|
| 1. Icon without transparency | **Done.** Every pixel was already opaque, so dropping the channel changed nothing visible. |
| 2. Privacy manifests | **Done**, for the app and the PushProvider extension. |
| 3. SIP certificate pinning | **Done and tested** against the native server from the iOS simulator: the right pin registers and the call carries audio; a wrong SIP-leg pin, with the gateway pinned right, is refused by the new check. |
| 4. Release builds without developer settings | **Done.** Decision 3 (the hidden page itself) is still open. |
| 5. Open-source notices | **Done**: Settings › About › Acknowledgements. |
| 6. Version and encryption key | Version **1.0 done.** Build numbers: let Xcode raise them at upload ("Manage Version and Build Number" when distributing). `ITSAppUsesNonExemptEncryption` **waits** for the export-compliance answer. |
| 7. Demo mode | **Waits** for decision 2 (and Apple's approval). |
| 8. Permission prompts | **Built.** Needs Matt's fresh-install check on a phone: the simulator does not enforce Local Network privacy. |
| Found on the way: device token in the log | **Fixed.** The SIP account line carried `auth_pass=<device token>` into the app log (Status page, diagnostics upload); every engine log line is now redacted. |

## Review access

App Review has to be able to sign in and use the app (guideline 2.1). The app
does nothing without an organisation's own server, which a reviewer cannot
reach. There are two ways to give them one.

### Option A: a public demo server

What it takes:

- A host with a public address and DNS name, running the call server with
  its enrolment, gateway (7443), SIP (5061) and RTP ports open to the
  internet.
- Something to call. The server has no echo or test extension of its own;
  the harness's answering phones are separate containers (baresip,
  Asterisk), so one of those runs beside it.
- Enrolment codes a reviewer can still use. Codes are claimable for 15
  minutes (`CodeTTL`, server/internal/enroll/enroll.go) and once only, and
  review takes days, so the server needs a review-code mechanism: a long-lived
  code, or several.
- Working on an IPv6-only network. Apple reviews on NAT64. The SIP and
  media stack (libre/baresip) opens its own sockets and runs its own DNS,
  so this has to be tested and probably fixed before a reviewer can
  register at all.
- Kept up, and not abused, for every review of every update: a public SIP
  server attracts scanners and toll-fraud attempts, so it needs no route
  to a real PBX or trunk.

### Option B: a demo mode in the app

Guideline 2.1 allows it: if a demo account cannot be provided, "you may
include a built-in demo mode in lieu of a demo account with prior approval
by Apple. Ensure the demo mode exhibits your app's full features and
functionality."

What it takes (app only; nothing on the server):

- A way in that is not a hidden feature: a documented enrolment code (for
  example `DEMODEMO`, typed in Enter Code Manually), named in the review
  notes. A "Try the Demo" button on the landing page would also do it, at
  some cost to its minimalism.
- A demo engine behind the existing `CallEngine` protocol (the controller
  already runs on fakes in its tests): outgoing calls ring for a moment,
  connect, and **echo the microphone back** once CallKit activates the
  audio session. That is real CallKit, a real audio path and the real
  in-call screen.
- An incoming demo call, so CallKit's ringing and answering can be shown
  too (for example a "Receive a Demo Call" row in Settings while in demo
  mode, ringing a few seconds later).
- A sample directory and Recents, the keypad, the Settings summary
  ("Demo" in place of the line), and Sign Out to leave the demo.
- Not demonstrable: background calls over office Wi-Fi (Local Push). The
  review notes say what they do and why a reviewer cannot trigger them.

### Comparison

| | A: public server | B: demo mode |
|---|---|---|
| Build work | Hosting, DNS, firewall, an answering endpoint, review codes on the server, the IPv6 fix | A demo engine with echo, the demo data, and a way in; app only |
| Blocking risk | The IPv6-only review network: the reviewer cannot even register until the SIP stack works on NAT64 | Apple's "prior approval" for demo mode, and whether the reviewer accepts that it shows the full app |
| Every later update | The server must be up, patched and holding valid codes for each review | Nothing; the demo ships in the app |
| Side benefit | A live system to show prospects | Anyone can try the app without a server, which is useful for prospects too |
| Rough effort | Several days, plus upkeep | 1–2 days |

**Recommendation: B, demo mode.** It is quicker, it keeps review away from
the IPv6 risk (which still wants fixing for real users, but no longer blocks
the first submission), and it costs nothing at each update. The one real
risk is approval, so ask App Review first, through App Store Connect
(Contact Us → App Review), describing the product and the demo mode. If
Apple insists on a live system, option A is still open, and the demo
engine is not wasted.

## Apple accounts and App Store Connect (Matt)

- **Developer Program** membership in the organisation's name (needs a
  D-U-N-S number).
- **Signing:** a distribution certificate and App Store profiles for the app
  (`com.latentbadger.dialler`) and the extension
  (`com.latentbadger.dialler.PushProvider`). App Groups and Network
  Extensions → App Push Provider must be on the distribution profiles as
  they are on the development ones.
- **Metadata:** description, keywords, support URL, **privacy policy URL**
  (required), category (Business), age rating, copyright, review contact
  and notes.
- **Screenshots:** the 6.9-inch iPhone size only; the app is iPhone-only
  (`TARGETED_DEVICE_FAMILY = 1`).
- **App Privacy label:** diagnostics and logs go to the customer's own
  server, not to us, so "Data Not Collected" is defensible; the privacy
  policy still says what the app sends and where.
- **Export compliance:** the app ships its own encryption (OpenSSL for TLS
  and SRTP) as well as Apple's, so the encryption questions must be answered.
  Probably standard mass-market encryption; confirm, and check whether
  France needs its own declaration.
- **Territories:** exclude mainland China (Apple does not allow CallKit
  apps there).

## Changes in the project

On a branch `feature/app-store-readiness`, merged on approval.

### Must fix before submitting

1. **App icon without transparency.** `AppIcon.appiconset/icon.png` has an
   alpha channel, which App Store Connect rejects; flatten it onto an opaque
   background (all three appearance slots use the same file).
2. **Privacy manifest** (`PrivacyInfo.xcprivacy`, missing). It must declare:
   UserDefaults (the app and its App Group: reasons CA92.1 and 1C8F.1);
   file timestamps (`stat`/`fstat` in the bundled libre and OpenSSL:
   C617.1); tracking: none; data collected: none (see the privacy label).
3. **SIP certificate checking.** `AppModel` always creates
   `BaresipCallEngine(acceptAnyCertificate: true)`, i.e.
   `sip_verify_server no`, even after enrolment has pinned the server's
   certificate. The SIP leg must be checked against the same pin. This is
   a security fix for a shipped product, not only a review matter.
   *Built:* the shim (`cbaresip.c`, `verify_server_cert`) replaces
   OpenSSL's chain check on baresip's SIP TLS context with a comparison of
   the leaf's SHA-256 against the enrolment pin, as the gateway connection
   does. Issuer, dates and name are not consulted, so self-signed
   certificates work, and nothing needs a publicly trusted CA.
   *Certificates:* one server certificate (self-signed, kept in
   `<data-dir>/tls`) serves the gateway, the phone API, SIP and the
   internal admin API; phones pin it, so it never needs renewing for them,
   and `<data-dir>/tls` belongs in backups (losing it means re-enrolling
   every phone). The admin console (`dialler-admin`) has its own
   certificate, which can be publicly trusted and renewed freely. Two
   server follow-ups: issue #15 (the console stops reaching the server
   when the server's self-signed certificate passes its one-year expiry)
   and issue #16 (pick up a renewed console certificate without a
   restart).
4. **Release builds without the developer settings:** Accept Any
   Certificate, manual device ID and token, and Local Push by hand
   (decision 3 settles the rest of the Status page).
5. **Open-source notices.** OpenSSL (Apache 2.0), libre and baresip (BSD),
   and Opus (BSD) require their notices to ship with the app: an
   Acknowledgements page under Settings › About.
6. **Version and encryption keys.** `ITSAppUsesNonExemptEncryption` in
   Info.plist once export compliance is settled; `MARKETING_VERSION` 1.0
   (it is 0.1.0) and a build number that rises with every upload
   (`CURRENT_PROJECT_VERSION` is 1).
7. **Demo mode** (if decision 2 is B), as described above.
8. **Permission prompts at the right moment, and no failure on first run.**
   - *Local network (found by Matt on device, 2026-09-28):* after a fresh
     install the first enrolment fails, then iOS asks for Local Network
     access; once allowed, the second attempt works. iOS has no call that
     asks for the permission; the prompt appears on the first local-network
     connection, and that connection (the enrolment request) fails at once
     rather than waiting for the answer. Fix: once the server's address is
     known (after the QR scan, or on Continue in Enter Code), open a plain
     test connection to it first (`NWConnection`, the approach in Apple's
     Local Network Privacy FAQ) and wait. It becomes ready when access is
     allowed, and enrolment then goes ahead, so the user never sees a
     failure. It waits with a policy-denied error when access is refused,
     and the app says so plainly, with a button to open Settings, instead
     of an enrolment error. For a public server there is no prompt and the
     test connection costs milliseconds. Device-only to verify: the
     simulator does not enforce Local Network privacy.
   - *Microphone:* the prompt appears at first launch, before setup
     (`CallKitBridge.requestMicrophonePermission` at provider
     registration); ask once enrolment succeeds instead.

### Should do

9. **`audio` background mode.** `UIBackgroundModes` has `audio` as well as
   `voip`; reviewers sometimes question it. Test whether CallKit calls work
   without it and drop it if they do.

## Tests before submitting

- **IPv6-only network (NAT64).** macOS Internet Sharing's NAT64 test network
  with a phone on it: enrolment, registration and a call. Required before
  submission for option A. For option B it does not block review, but real
  customers deserve it before long.
- **An Archive build from the current release Xcode** (not a beta), uploaded
  and installed through **TestFlight**: on a fresh install, the Local
  Network prompt before the first enrolment and no failed attempt;
  enrolment by QR and by code,
  incoming and outgoing calls, background calls on office Wi-Fi, sign out,
  and the demo mode end to end if there is one.

## Order

1. Settle decisions 2 and 3; for demo mode, ask App Review.
2. Must-fixes 1–6 and 8 (and 7) on `feature/app-store-readiness`,
   typechecked and screenshotted; 8 confirmed on a freshly installed phone.
3. The IPv6 test (and fix, for option A).
4. Option A only: the demo server and review codes.
5. TestFlight.
6. Metadata, screenshots, privacy label, export compliance, review notes.
7. Submit.
