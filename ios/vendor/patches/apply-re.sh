#!/bin/sh
# In-place, idempotent source patches for libre. Usage: apply-re.sh <re-src-dir>
#
# re_main(): do not spin on EBADF. Upstream's Darwin "workaround" retries
# fd_poll() forever when kevent() fails with EBADF, without running timers
# or sleeping — the state the loop is in once its kqueue descriptor has been
# closed (context torn down under it, or its descriptor number closed twice
# by someone else). On iOS that is the engine thread at 100 % CPU in the
# background until the system kills the app (cpu_resource_fatal, 48 s at
# 99 %, 2026-09-12). Now: warn with the descriptor state so the next
# occurrence names the cause, retry a bounded number of times (a transient
# EBADF is still tolerated), then return EBADF — the shim's loop thread
# treats an unasked return as "loop dead" and the engine rebuilds the stack.
set -eu
SRC="$1"

if ! grep -q 'Dialler: EBADF' "$SRC/src/main/main.c"; then
  perl -0pi -e 's/(\tre_lock\(re\);\n\tfor \(;;\) \{\n)/\tunsigned ebadf = 0; \/* Dialler: EBADF spin guard *\/\n$1/;
    s/#ifdef DARWIN\n\t\t\t\/\* NOTE: workaround for Darwin \*\/\n\t\t\tif \(EBADF == err\)\n\t\t\t\tcontinue;\n\n#endif\n/#ifdef DARWIN\n\t\t\t\/* NOTE: workaround for Darwin — bounded (Dialler: EBADF) *\/\n\t\t\tif (EBADF == err) {\n\t\t\t\tif (++ebadf <= 8) {\n\t\t\t\t\tDEBUG_WARNING("re_main: fd_poll EBADF %u\/8 (kqfd=%d nfds=%d)\\n", ebadf, re->kqfd, re->nfds);\n\t\t\t\t\tsys_msleep(10);\n\t\t\t\t\tcontinue;\n\t\t\t\t}\n\t\t\t\tDEBUG_WARNING("re_main: giving up after repeated EBADF (kqfd=%d)\\n", re->kqfd);\n\t\t\t\tbreak;\n\t\t\t}\n#endif\n/;' "$SRC/src/main/main.c"
  grep -q 're_sys.h' "$SRC/src/main/main.c" || perl -pi -e 's/^(#include <re_mem\.h>\n)/$1#include <re_sys.h> \/* Dialler: EBADF guard sleeps *\/\n/' "$SRC/src/main/main.c"
  grep -q 'Dialler: EBADF' "$SRC/src/main/main.c" || { echo "patch: re main.c anchor not found"; exit 1; }
  grep -q 'giving up after repeated EBADF' "$SRC/src/main/main.c" || { echo "patch: re main.c EBADF anchor not found"; exit 1; }
fi
echo "   re: re_main EBADF guard present"

# QoS on Apple (plan Phase E). udp_settos()/tcp_settos()/tcp_conn_settos()
# set IP_TOS only. On iOS the DSCP byte alone does not select the Wi-Fi
# access category; the socket's SO_NET_SERVICE_TYPE does (NET_SERVICE_TYPE_VO
# → WMM AC_VO for voice, _SIG for signalling), and IPv6 sockets need
# IPV6_TCLASS. baresip applies rtp_tos (184 = EF) to RTP/RTCP and sip_tos
# to the SIP transports through these three functions.
if ! grep -q 'Dialler: net service type' "$SRC/src/udp/udp.c"; then
  perl -0pi -e 's/(\terr = udp_setsockopt\(us, IPPROTO_IP, IP_TOS, &v, sizeof\(v\)\);\n\treturn err;\n\})/\terr = udp_setsockopt(us, IPPROTO_IP, IP_TOS, &v, sizeof(v));\n#ifdef DARWIN\n\t\/* Dialler: net service type — the option iOS maps to the Wi-Fi access\n\t * category; IP_TOS alone does not. IPv6 sockets carry the DSCP in the\n\t * traffic class. Failures are ignored: the socket may be the other\n\t * family, and marking is best effort. *\/\n\t{\n\t\tint nst = tos >= 184 ? 4 \/* NET_SERVICE_TYPE_VO *\/ :\n\t\t\t  tos >= 96 ? 2 \/* NET_SERVICE_TYPE_SIG *\/ : 0;\n\t\tint tc = tos;\n\t\t(void)udp_setsockopt(us, SOL_SOCKET, 0x1116 \/* SO_NET_SERVICE_TYPE *\/, &nst, sizeof(nst));\n\t\t(void)udp_setsockopt(us, IPPROTO_IPV6, IPV6_TCLASS, &tc, sizeof(tc));\n\t\terr = 0;\n\t}\n#endif\n\treturn err;\n}/' "$SRC/src/udp/udp.c"
  grep -q 'Dialler: net service type' "$SRC/src/udp/udp.c" || { echo "patch: udp.c settos anchor not found"; exit 1; }
fi
if ! grep -q 'Dialler: net service type' "$SRC/src/tcp/tcp.c"; then
  perl -0pi -e 's/(\tts->tos = tos;\n\terr = tcp_sock_setopt\(ts, IPPROTO_IP, IP_TOS, &v, sizeof\(v\)\);\n)/$1#ifdef DARWIN\n\t{\t\/* Dialler: net service type (see udp.c) *\/\n\t\tint nst = tos >= 184 ? 4 : tos >= 96 ? 2 : 0;\n\t\tint tc = (int)tos;\n\t\t(void)tcp_sock_setopt(ts, SOL_SOCKET, 0x1116, &nst, sizeof(nst));\n\t\t(void)tcp_sock_setopt(ts, IPPROTO_IPV6, IPV6_TCLASS, &tc, sizeof(tc));\n\t\terr = 0;\n\t}\n#endif\n/;
    s/(\ttc->tos = tos;\n\tif \(tc->fdc != RE_BAD_SOCK\) \{\n\t\tif \(0 != setsockopt\(tc->fdc, IPPROTO_IP, IP_TOS,\n\t\t\t\t\tBUF_CAST &v, sizeof\(v\)\)\)\n\t\t\terr = RE_ERRNO_SOCK;\n)/$1#ifdef DARWIN\n\t\t{\t\/* Dialler: net service type (see udp.c) *\/\n\t\t\tint nst = tos >= 184 ? 4 : tos >= 96 ? 2 : 0;\n\t\t\tint tc2 = (int)tos;\n\t\t\t(void)setsockopt(tc->fdc, SOL_SOCKET, 0x1116, BUF_CAST &nst, sizeof(nst));\n\t\t\t(void)setsockopt(tc->fdc, IPPROTO_IPV6, IPV6_TCLASS, BUF_CAST &tc2, sizeof(tc2));\n\t\t\terr = 0;\n\t\t}\n#endif\n/;' "$SRC/src/tcp/tcp.c"
  grep -c 'Dialler: net service type' "$SRC/src/tcp/tcp.c" | grep -q 2 || { echo "patch: tcp.c settos anchors not found"; exit 1; }
fi
echo "   re: Apple net-service-type QoS patch present"

# ---- patch level ------------------------------------------------------------
# Exported so the app can log which libre it was linked against; bump when a
# patch above changes. cb_version() prints it as "libre patch level N".
#   1: re_main EBADF guard   2: + Apple SO_NET_SERVICE_TYPE / IPV6_TCLASS marks
RE_PATCH_LEVEL=2
if ! grep -q 're_dialler_patchlevel' "$SRC/src/main/main.c"; then
  printf '\n/* Dialler: patch level, see ios/vendor/patches/apply-re.sh */\nint re_dialler_patchlevel(void)\n{\n\treturn %s;\n}\n' "$RE_PATCH_LEVEL" >> "$SRC/src/main/main.c"
fi
grep -q "return $RE_PATCH_LEVEL;" "$SRC/src/main/main.c" || perl -pi -e 's/(int re_dialler_patchlevel\(void\)\n\{\n\treturn )\d+;/${1}'"$RE_PATCH_LEVEL"';/' "$SRC/src/main/main.c"
echo "   re: patch level $RE_PATCH_LEVEL"
