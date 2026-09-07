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
# on recorded audio (app → desk phone, desk phone → app).
harness-trunk:
	sh harness/trunk_test.sh

# Echo self-test: the app hears its own audio back from the server
# ("echo", app leg only) or from the PBX ("600", through the trunk).
harness-echo:
	sh harness/echo_test.sh
	TRUNK=1 sh harness/echo_test.sh

# Ring the iOS app (simulator or device, connected as dev-a) through the server.
harness-ring-sim:
	sh harness/ring_sim.sh

# Device testing without Docker's UDP proxy in the media path: the server
# runs natively on this Mac (media ports on en0), provisioned with the same
# fixed dev tokens; the docker caller registers to it over outbound NAT.
#   make dev-server            # terminal 1 (Ctrl-C to stop)
#   make harness-ring-native   # terminal 2: 202 dials the phone
dev-server: server tone
	@echo "native server on $(DIALLER_PUBLIC_HOST); app Settings: host $(DIALLER_PUBLIC_HOST), port 7443, dev-a / tok_dev_a_harness_fixed"
	( sleep 2 && DIALLER_API=http://127.0.0.1:8080 sh harness/provision.sh ) &
	./bin/dialler-server -data-dir ./data -admin-token harness -public-host $(DIALLER_PUBLIC_HOST) -local-domain dialler \
	  -http-addr 0.0.0.0:8080 -ring-timeout 30s -rtp-min 20000 -rtp-max 20100 $(DEV_SERVER_FLAGS)
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

# Package tests on macOS (no device).
ios-test:
	cd ios/DiallerProtocol && swift test
	cd ios/DiallerCore && swift test

# Compile the packages for the simulator and type-check the app + extension
# sources against the iOS SDK without Xcode's package resolution. Useful in
# restricted sandboxes; the real build is `xcodebuild -scheme Dialler`.
ios-typecheck:
	cd ios/DiallerEngine && swift build --disable-sandbox --scratch-path "$(IOS_SCRATCH)" --triple $(IOS_TRIPLE) --sdk "$(IOS_SDK)" --target DiallerEngine 2>&1 | grep -vE 'Wincomplete-umbrella|Wvisibility' || true
	cd ios/Dialler && swiftc -typecheck -parse-as-library -swift-version 5 -target $(IOS_TRIPLE) -sdk "$(IOS_SDK)" \
	  -I "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/Modules" -I "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/CBaresip.build" App/*.swift
	cd ios/Dialler && swiftc -typecheck -parse-as-library -swift-version 5 -target $(IOS_TRIPLE) -sdk "$(IOS_SDK)" \
	  -I "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/Modules" PushProvider/*.swift

# Cross-compile libre/baresip/Opus/OpenSSL for iOS device, simulator and macOS
# into ios/vendor/xcframeworks (required once before the app links
# DiallerEngine and before engine-probe; ~15 min).
ios-vendor:
	sh ios/vendor/build-baresip.sh all all

# Headless run of the real baresip engine on this Mac against the docker
# server: registers 201, answers a call if one arrives. Needs the macOS slice
# (ios-vendor) and a server started with DIALLER_PUBLIC_HOST=<mac-ip>.
#   make engine-probe HOST=$$(ipconfig getifaddr en0)
HOST ?= 127.0.0.1
engine-probe:
	cd ios/DiallerEngine && swift run --disable-sandbox engine-probe $(HOST) 201@dialler 5061 40

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
#   OUTBOUND=202 make sim-call       # the simulated phone dials (keypad / directory path)
#   OUTBOUND=echo make sim-call      # full mic→server→speaker loop (server echo)
#   HOLD_MS=3000 make sim-call       # hold/resume mid-call, asserts media stops and restarts
#   TRANSFER=echo make sim-call      # blind transfer of the caller to echo (REFER handled server-side)
#   GATEWAY=no make sim-call         # no wake ever arrives; rings from the INVITE
#   SERVER=native make sim-call      # against `make dev-server` on this Mac
sim-call:
	sh harness/sim_call.sh

# The outbound + incoming pair every call-path change must pass.
sim-call-all:
	OUTBOUND=202 sh harness/sim_call.sh
	CALLS=2 sh harness/sim_call.sh

SIM ?= iPhone 16
audio-probe-sim:
	cd ios/DiallerEngine && swift build --disable-sandbox --scratch-path "$(IOS_SCRATCH)" --triple $(IOS_TRIPLE) --sdk "$(IOS_SDK)" --product audio-probe 2>&1 | grep -vE 'Wincomplete-umbrella|Wvisibility' || true
	xcrun simctl boot "$(SIM)" 2>/dev/null || true
	xcrun simctl spawn "$(SIM)" "$(IOS_SCRATCH)/arm64-apple-ios-simulator/debug/audio-probe"

# Build for the simulator with Xcode (needs an unrestricted shell).
ios-build:
	cd ios/Dialler && xcodebuild -project Dialler.xcodeproj -scheme Dialler -configuration Debug \
	  -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build
