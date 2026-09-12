# Dev/test PBX peer (SPEC §3). Debian 12 no longer packages Asterisk, so we
# use Alpine's package, which is multi-arch (arm64 included). The sample
# config supplies asterisk.conf/modules.conf; we overlay our own dial plan
# and PJSIP config.
FROM alpine:3.20
RUN apk add --no-cache asterisk asterisk-sample-config asterisk-sounds-en
COPY pjsip.conf extensions.conf /etc/asterisk/
# ASTERISK_CODECS: what every endpoint allows, in preference order (default
# g722,ulaw,alaw). "ulaw,alaw" builds a narrowband-only PBX — the shape of
# a customer exchange without G.722 — for `NARROWBAND=1 make harness-trunk`.
ARG ASTERISK_CODECS=g722,ulaw,alaw
RUN allow="$(printf 'allow=%s\n' $(echo "$ASTERISK_CODECS" | tr ',' ' '))" \
 && awk -v allow="$allow" '/^allow=/ { if (!done) { printf "%s\n", allow; done=1 } next } /^\[/ { done=0 } { print }' \
      /etc/asterisk/pjsip.conf > /etc/asterisk/pjsip.conf.new \
 && mv /etc/asterisk/pjsip.conf.new /etc/asterisk/pjsip.conf \
 && grep -c "^allow=" /etc/asterisk/pjsip.conf
RUN chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/log/asterisk /var/spool/asterisk /var/run/asterisk 2>/dev/null || true
EXPOSE 5060/udp 5060/tcp
CMD ["asterisk", "-f", "-vvv", "-U", "asterisk", "-G", "asterisk"]
