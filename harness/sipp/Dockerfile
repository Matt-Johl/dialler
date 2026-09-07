# SIPp for app-leg conformance scenarios (SPEC §5 row 3).
# Debian's sip-tester package is built without OpenSSL and cannot speak TLS,
# so SIPp is compiled here with USE_SSL. Two stages keep the runtime slim.
FROM debian:bookworm-slim AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates pkg-config libssl-dev libncurses-dev \
 && rm -rf /var/lib/apt/lists/*
ARG SIPP_VERSION=v3.7.3
RUN git clone --depth 1 --branch "$SIPP_VERSION" --recurse-submodules https://github.com/SIPp/sipp.git /sipp
WORKDIR /sipp
RUN cmake . -DUSE_SSL=1 -DUSE_PCAP=0 -DUSE_SCTP=0 -DUSE_GSL=0 -DBUILD_STATIC=0 \
 && make -j"$(nproc)" \
 && ./sipp -v | head -n1

FROM debian:bookworm-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      libssl3 libncurses6 libncursesw6 libtinfo6 openssl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /sipp/sipp /usr/local/bin/sipp
WORKDIR /work
# SIPp's TLS mode wants a client cert pair in the working directory.
RUN openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
      -keyout cakey.pem -out cacert.pem -days 365 -subj "/CN=sipp-harness"
COPY run.sh /work/run.sh
RUN chmod +x /work/run.sh
ENTRYPOINT ["/work/run.sh"]
