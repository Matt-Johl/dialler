# Packet-capture sidecar for the QoS gate: shares the server's network
# namespace and counts DSCP-marked packets the server sends.
FROM alpine:3.20
RUN apk add --no-cache tcpdump
COPY capture.sh /capture.sh
ENTRYPOINT ["sh", "/capture.sh"]
