# Dev/test PBX peer (SPEC §3). Debian 12 no longer packages Asterisk, so we
# use Alpine's package, which is multi-arch (arm64 included). The sample
# config supplies asterisk.conf/modules.conf; we overlay our own dial plan
# and PJSIP config.
FROM alpine:3.20
RUN apk add --no-cache asterisk asterisk-sample-config asterisk-sounds-en
COPY pjsip.conf extensions.conf /etc/asterisk/
RUN chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/log/asterisk /var/spool/asterisk /var/run/asterisk 2>/dev/null || true
EXPOSE 5060/udp 5060/tcp
CMD ["asterisk", "-f", "-vvv", "-U", "asterisk", "-G", "asterisk"]
