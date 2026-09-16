.PHONY: all test go-test swift-test vet fmt server run harness-up harness-down harness-test tone

all: test

test: go-test swift-test

go-test:
	cd server && go vet ./... && go test ./...

swift-test:
	cd ios/DiallerProtocol && swift test

vet:
	cd server && go vet ./...

fmt:
	cd server && gofmt -l -w .

server:
	cd server && go build -o ../bin/dialler-server ./cmd/dialler-server

# Dev run: self-signed TLS, data in ./data, admin token printed in the log.
run: server
	./bin/dialler-server -data-dir ./data -admin-token dev

tone:
	python3 harness/baresip/media/gen_tone.py

# The address the server advertises to phones for SIP and media. Defaults to
# this Mac's en0 address so real devices work; inside-docker-only tests are
# fine with it too because the media ports are published on the host.
export DIALLER_PUBLIC_HOST ?= $(shell ipconfig getifaddr en0 2>/dev/null || ifconfig en0 2>/dev/null | awk '/inet /{print $$2; exit}' || echo dialler)

harness-up: tone
	@echo "advertising public host $(DIALLER_PUBLIC_HOST)"
	docker compose -f harness/docker-compose.yml up --build -d dialler asterisk
	sleep 2 && sh harness/innet.sh dialler-harness_default harness/provision.sh

harness-test:
	docker compose -f harness/docker-compose.yml --profile test run --rm --build sipp

harness-down:
	docker compose -f harness/docker-compose.yml --profile test down -v

# Headless app↔app call through the real server (registrar + wake path +
# diago bridge), asserted on the callee's recorded audio.
harness-call:
	sh harness/call_test.sh

# Callee registered, then its connection dies: the server must not dial the
# stale route but fall back to the wake path.
harness-flow-gone:
	sh harness/flow_gone_test.sh

# NAT regression (macOS host, not sandboxable): the real engine on this Mac
# registers through Docker's port forwarding — a genuine NAT — and must be
# reached over its own TLS connection with symmetric RTP.
#   make probe-call                                 # PASS
#   DIALLER_REWRITE_CONTACT=false make probe-call   # must FAIL (the bug this guards)
probe-call:
	sh harness/probe_call.sh

# Wake path: callee starts unregistered, a fake app on the gateway receives
# the wake and registers the phone, then the bridge completes.
harness-wake:
	sh harness/wake_test.sh

# PBX leg: the server as Asterisk's SIP trunk peer, both directions, asserted
# on recorded audio (app → desk phone, desk phone → app), plus a transfer of
# the desk phone to a PBX extension that the server must hand to Asterisk
# and drop out of (DIRECTION=out|in|transfer for one of them).
harness-trunk:
	sh harness/trunk_test.sh

# Hold music (SPEC §4.4 rule 8b): the app-leg caller holds a trunk call and
# the desk phone must hear the server's music, not silence. The caller plays
# silence throughout, so anything the desk phone records is the music.
harness-hold-music:
	sh harness/hold_music_test.sh

# A PBX extension with no registration must be refused by the PBX (480),
# not rung: the dialplan's dial-status context turns Dial()'s outcome into a
# SIP status. Publishes no host ports, so it is safe beside `make dev-server`.
harness-pbx-unavailable:
	sh harness/pbx_unavailable_test.sh

# Echo self-test and audio-quality gate (SPEC §7.2): the app hears its own
# audio back from the server ("echo", app leg only) or from the PBX ("600",
# through the trunk); the recording is correlated with the played tone for
# mouth-to-ear delay (≤150 ms / ≤200 ms) and gaps (none). IMPAIR=1 adds
# 2% loss + 30±10 ms jitter to what the server sends and judges concealment.
#   IMPAIR=1 make harness-echo      MAX_GAP_MS=… MAX_GAPS=… to relax a bound
# QoS gate (SPEC §4.4): a tcpdump sidecar asserts the server marks media
# DSCP EF and signalling CS3.
harness-qos:
	sh harness/qos_test.sh

harness-echo:
	sh harness/echo_test.sh
	TRUNK=1 sh harness/echo_test.sh

# Ring the iOS app (simulator or device, connected as dev-a) through the server.
harness-ring-sim:
	sh harness/ring_sim.sh

# Device testing without Docker's UDP proxy in the media path: the server
# runs natively on this Mac (media ports on en0), provisioned with the same
# fixed dev tokens; the docker caller registers to it over outbound NAT.
# The PBX for real SIP phones is a standalone Asterisk on an Ubuntu box on
# the LAN (harness/asterisk-native/README.md); pass its address as
# ASTERISK_HOST to trunk to it, or leave it unset for app-only testing.
#   make dev-server                          # terminal 1 (Ctrl-C to stop)
#   ASTERISK_HOST=10.18.0.50 make dev-server # trunked to the Ubuntu PBX
#   make harness-ring-native                 # terminal 2: 212 (docker) dials the phone
ASTERISK_HOST ?=
TRUNK_FLAGS = $(if $(ASTERISK_HOST),-trunk "sip:$(ASTERISK_HOST):5060;transport=udp" -trunk-addr :5062 -trunk-external-host $(DIALLER_PUBLIC_HOST),)
dev-server: server tone
	@echo "native server on $(DIALLER_PUBLIC_HOST); app Settings: host $(DIALLER_PUBLIC_HOST), port 7443, dev-a / tok_dev_a_harness_fixed"
	@echo "trunk: $(if $(ASTERISK_HOST),Asterisk at $(ASTERISK_HOST):5060; trunk listener :5062,none (set ASTERISK_HOST=<ubuntu-ip> for the PBX))"
	( sleep 2 && DIALLER_API=https://127.0.0.1:8080 sh harness/provision.sh ) &
	@mkdir -p data/logs
	./bin/dialler-server -data-dir ./data -admin-token harness -public-host $(DIALLER_PUBLIC_HOST) -local-domain dialler \
	  -http-addr 0.0.0.0:8080 -ring-timeout 30s -rtp-min 20000 -rtp-max 20100 $(TRUNK_FLAGS) $(DEV_SERVER_FLAGS) \
	  2>&1 | tee -a data/logs/dev-server.log
# The server log is also kept in data/logs/dev-server.log, and devices upload
# their own logs and crash reports to data/diag/<device>/ (ios/README.md).
# e.g. DEV_SERVER_FLAGS="-log-level debug" make dev-server   (shows sipgo's connection reference counting)

harness-ring-native:
	sh harness/ring_native.sh

# Same call through the standalone engine-spike B2BUA in ./spike.
spike-call:
	sh harness/spike/call_test.sh

# ---- iOS ---------------------------------------------------------------------
IOS_SDK = $(shell xcrun --sdk iphonesimulator --show-sdk-path)
IOS_TRIPLE = arm64-apple-ios17.0-simulator
IOS_SCRATCH = $${TMPDIR:-/tmp}/spm-build-ios
# SwiftPM and clang write module caches under ~/Library by default, which
# the build sandbox refuses — including for the *manifest* compile, so a
# package cannot even be read without this. Harmless outside the sandbox.
# (Surfaced 2026-09-16 when an Xcode update invalidated the existing caches.)
SWIFT_CACHE_ENV = SWIFTPM_MODULECACHE_OVERRIDE="$${TMPDIR:-/tmp}/spm-modcache" \
  CLANG_MODULE_CACHE_PATH="$${TMPDIR:-/tmp}/clang-modcache"
# Where SwiftPM leaves the built .swiftmodules. The layout moved with the
# Xcode that ships Swift 6.4 (flat `debug/`, plus an Xcode-style `out/`);
# earlier toolchains used `<triple>/debug/Modules`. swiftc ignores an -I
# that does not exist, so naming all three keeps `make ios-typecheck`
# working across a toolchain update instead of failing with "no such
# module" (2026-09-16).
IOS_MODULE_PATHS = -I "$(IOS_SCRATCH)/debug" \
  -I "$(IOS_SCRATCH)/out/Products/Debug-iphonesimulator" \
  -I "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/Modules" \
  -I "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/CBaresip.build" \
  $(IOS_CMODULE_MAPS)
# The C module (CBaresip) is reached through a module map, and the newer
# toolchain generates it as `<Module>.modulemap` rather than the
# `module.modulemap` that -I looks for — so point swiftc straight at each
# one. Empty on the older layout, where -I above is enough.
IOS_CMODULE_MAPS = $(foreach m,\
  $(wildcard $(TMPDIR)/spm-build-ios/out/Intermediates.noindex/GeneratedModuleMaps-iphonesimulator/CBaresip.modulemap),\
  -Xcc -fmodule-map-file=$(m))

# Package tests on macOS (no device).
ios-test:
	cd ios/DiallerProtocol && $(SWIFT_CACHE_ENV) swift test
	cd ios/DiallerCore && $(SWIFT_CACHE_ENV) swift test

# Compile the packages for the simulator and type-check the app + extension
# sources against the iOS SDK without Xcode's package resolution. Useful in
# restricted sandboxes; the real build is `xcodebuild -scheme Dialler`.
ios-typecheck:
	cd ios/DiallerEngine && $(SWIFT_CACHE_ENV) swift build --disable-sandbox --scratch-path "$(IOS_SCRATCH)" --triple $(IOS_TRIPLE) --sdk "$(IOS_SDK)" --target DiallerEngine 2>&1 | grep -vE 'Wincomplete-umbrella|Wvisibility' || true
	cd ios/Dialler && $(SWIFT_CACHE_ENV) swiftc -typecheck -parse-as-library -swift-version 5 -target $(IOS_TRIPLE) -sdk "$(IOS_SDK)" -module-cache-path "$${TMPDIR:-/tmp}/swift-modcache" \
	  $(IOS_MODULE_PATHS) App/*.swift
	cd ios/Dialler && $(SWIFT_CACHE_ENV) swiftc -typecheck -parse-as-library -swift-version 5 -target $(IOS_TRIPLE) -sdk "$(IOS_SDK)" -module-cache-path "$${TMPDIR:-/tmp}/swift-modcache" \
	  $(IOS_MODULE_PATHS) PushProvider/*.swift

# Cross-compile libre/baresip/Opus/OpenSSL for iOS device, simulator and macOS
# into ios/vendor/xcframeworks (required once before the app links
# DiallerEngine and before engine-probe; ~15 min).
ios-vendor:
	sh ios/vendor/build-baresip.sh all all

# Host-side unit test of the packet-loss concealment compiled into the
# app's (and the harness phone's) G.711 and G.722 modules.
# Symbolicate an iOS crash / CPU report (.ips) or a MetricKit diagnostic
# (JSON, as uploaded to data/diag/) against the build symbols the Xcode
# "Keep build symbols" phase saves in ios/dsyms/<UUID>/.
symbolicate:
	python3 tools/symbolicate.py "$(FILE)"

plc-test:
	cc -std=c99 -Wall -Wextra -O2 -o "$${TMPDIR:-/tmp}/plc_test" ios/vendor/patches/plc/plc.c ios/vendor/patches/plc/plc_test.c -lm && "$${TMPDIR:-/tmp}/plc_test"

# Headless run of the real baresip engine on this Mac against the docker
# server: registers 203 (dev-s, the simulator/probe identity), answers a call if one arrives. Needs the macOS slice
# (ios-vendor) and a server started with DIALLER_PUBLIC_HOST=<mac-ip>.
#   make engine-probe HOST=$$(ipconfig getifaddr en0)
HOST ?= 127.0.0.1
engine-probe:
	cd ios/DiallerEngine && swift run --disable-sandbox engine-probe $(HOST) 203@dialler 5061 40 dev-s tok_dev_s_harness_fixed

# Does the audiounit driver move audio under the CallKit contract (units
# start only on release, in every event ordering)? No SIP, no server.
# macOS uses the HAL output unit; the simulator run uses VoiceProcessingIO,
# the same unit as the device. Both must PASS after any driver or engine
# audio change.
audio-probe:
	cd ios/DiallerEngine && swift run --disable-sandbox audio-probe

# The app's call path end to end with no phone: real engine + driver on the
# iOS simulator, real server, phone-b plays a tone; asserted on RTP received
# and rendered audio energy. Needs `make harness-up` first.
#   make sim-call                    # activation ~150ms after answer (device order)
#   ACTIVATE_MS=1500 make sim-call   # activation after the call established
#   CALLS=3 ANSWER_MS=8000 make sim-call   # several calls in one process, long rings
#   OUTBOUND=212 make sim-call       # the simulated phone dials (keypad / directory path)
#   OUTBOUND=echo make sim-call      # full mic→server→speaker loop (server echo)
#   HOLD_MS=3000 make sim-call       # hold/resume mid-call, asserts media stops and restarts
#   TRANSFER=echo make sim-call      # blind transfer of the caller to echo (REFER handled server-side)
#   DECLINE=1 make sim-call          # reject from the banner; asserts phone-b is told 486 Busy Here, not 480
#   RESTART_SERVER=1 make sim-call   # server restarts under the app: it must reconnect, re-register, take the call
#   GATEWAY=no make sim-call         # no wake ever arrives; rings from the INVITE
#   SERVER=native make sim-call      # against `make dev-server` on this Mac
sim-call:
	sh harness/sim_call.sh

# Call waiting (plan Phase I): a second caller while on a call, Hold &
# Accept, swap back, end both.
sim-call-cw:
	CALLWAITING=1 sh harness/sim_call.sh

# An outgoing call the server refuses (plan Phase J): the caller must hear
# the congestion tone and the call must not disappear until it has run.
sim-call-refused:
	OUTBOUND=999 REFUSED=1 sh harness/sim_call.sh

# The incoming, outbound, decline, refused, reconnect and call-waiting runs
# every call-path change must pass.
sim-call-all:
	OUTBOUND=212 sh harness/sim_call.sh
	CALLS=2 sh harness/sim_call.sh
	DECLINE=1 sh harness/sim_call.sh
	OUTBOUND=999 REFUSED=1 sh harness/sim_call.sh
	RESTART_SERVER=1 sh harness/sim_call.sh
	CALLWAITING=1 sh harness/sim_call.sh

SIM ?= iPhone 16
audio-probe-sim:
	cd ios/DiallerEngine && $(SWIFT_CACHE_ENV) swift build --disable-sandbox --scratch-path "$(IOS_SCRATCH)" --triple $(IOS_TRIPLE) --sdk "$(IOS_SDK)" --product audio-probe 2>&1 | grep -vE 'Wincomplete-umbrella|Wvisibility' || true
	xcrun simctl boot "$(SIM)" 2>/dev/null || true
	xcrun simctl spawn "$(SIM)" "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/audio-probe"

# Build for the simulator with Xcode (needs an unrestricted shell).
ios-build:
	cd ios/Dialler && xcodebuild -project Dialler.xcodeproj -scheme Dialler -configuration Debug \
	  -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build
