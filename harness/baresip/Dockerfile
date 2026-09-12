# Headless baresip phone (SPEC §7.1 audio seam, automated sibling). Audio in
# and out are WAV files via the aufile module, so a call's media can be
# asserted on by comparing recordings.
#
# Built from source: Debian 12/13 package baresip 1.0/1.1 (2020-21), whose
# aufile module cannot act as an audio source in our tests and which lacks
# the WAV-recording player. Current baresip (3.x) has both.
ARG BARESIP_VERSION=v3.15.0

FROM debian:bookworm-slim AS build
ARG BARESIP_VERSION
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates pkg-config \
      libssl-dev libopus-dev zlib1g-dev libspandsp-dev \
 && rm -rf /var/lib/apt/lists/*
# libspandsp (LGPL) backs baresip's g722 module here. Fine in this test
# container, where it is a shared library; the app does not link it — its
# G.722 is the public-domain implementation in ios/vendor/patches/g722.
WORKDIR /src
# libre and baresip release in lockstep with matching version tags.
RUN git clone --depth 1 --branch "$BARESIP_VERSION" https://github.com/baresip/re.git \
 && git clone --depth 1 --branch "$BARESIP_VERSION" https://github.com/baresip/baresip.git
# The same in-place source patches the app's build applies (build context is
# the repo root for this): the receive-path packet-loss concealment is what
# the audio-quality gates measure, so the harness phone must carry it too.
COPY ios/vendor/patches/apply-baresip.sh /src/apply-baresip.sh
RUN sh /src/apply-baresip.sh /src/baresip
RUN cmake -S re -B re/build -DCMAKE_BUILD_TYPE=Release \
 && cmake --build re/build -j"$(nproc)" \
 && cmake --install re/build
RUN cmake -S baresip -B baresip/build -DCMAKE_BUILD_TYPE=Release \
      -DSTATIC=OFF -DMODULES="account;contact;menu;ctrl_tcp;aufile;ausine;opus;g722;g711;ice;stdio;auconv;auresamp" \
 && cmake --build baresip/build -j"$(nproc)" \
 && cmake --install baresip/build

FROM debian:bookworm-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libssl3 libopus0 zlib1g ca-certificates libspandsp2 \
 && rm -rf /var/lib/apt/lists/* \
 && ldconfig
COPY --from=build /usr/local/ /usr/local/
RUN ldconfig
COPY harness/baresip/config harness/baresip/accounts harness/baresip/entrypoint.sh /opt/baresip/
RUN chmod +x /opt/baresip/entrypoint.sh
VOLUME ["/media"]
ENTRYPOINT ["/opt/baresip/entrypoint.sh"]
