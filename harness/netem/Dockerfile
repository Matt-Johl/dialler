# Network impairment for the audio-quality gates (SPEC §7.2): a sidecar
# that shares another service's network namespace and shapes its egress
# with tc/netem. Built once so tests never need internet access at run time.
FROM alpine:3.20
RUN apk add --no-cache iproute2
COPY impair.sh /impair.sh
ENTRYPOINT ["sh", "/impair.sh"]
