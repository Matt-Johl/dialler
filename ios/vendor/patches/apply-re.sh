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

# A flushed SIP connection must not be able to receive. sip_transp_flush()
# only drops the hash's reference on each connection and frees the
# transports; a connection pinned by a transaction in flight — a 486 waiting
# for its ACK on TCP/TLS keeps its request for Timer H, and the request keeps
# the connection — stays open and listening with its transports gone. The
# next packet on it completes that transaction inside the receive handler,
# the packet's own reference goes with mem_deref(msg), the connection is
# destroyed under the handler's feet, and the handler reads conn->mb->end
# from freed memory (transp.c tcp_recv_handler, after sip_recv). On iOS,
# whose allocator zeroes freed blocks, that is SIGSEGV at NULL+0x18: four
# crashes on 2026-09-17 16:29, each a reset run while a 486's ACK was in
# flight. Now every connection is closed before the flush: its socket goes
# away, queued sends fail through their handlers, and a pinned connection
# dies quietly when its transaction ends.
if ! grep -q 'Dialler: close before flush' "$SRC/src/sip/transp.c"; then
  perl -0pi -e 's/(void sip_transp_flush\(struct sip \*sip\)\n\{\n\tif \(!sip\)\n\t\treturn;\n\n)(\thash_flush\(sip->ht_conn\);\n)/\/* Dialler: close before flush, see ios\/vendor\/patches\/apply-re.sh *\/\nstatic bool flush_conn_close_handler(struct le *le, void *arg)\n{\n\tstruct sip_conn *conn = le->data;\n\t(void)arg;\n\n\tconn_close(conn, ECONNRESET);\n\tmem_deref(conn);\n\n\treturn false;\n}\n\n\n$1\thash_apply(sip->ht_conn, flush_conn_close_handler, NULL);\n$2/' "$SRC/src/sip/transp.c"
  grep -q 'Dialler: close before flush' "$SRC/src/sip/transp.c" || { echo "patch: re transp.c sip_transp_flush anchor not found"; exit 1; }
  grep -q 'hash_apply(sip->ht_conn, flush_conn_close_handler, NULL);' "$SRC/src/sip/transp.c" || { echo "patch: re transp.c flush call not inserted"; exit 1; }
fi
echo "   re: close-before-flush in sip_transp_flush present"

# A descriptor the kqueue reports ready but nobody handles must not be left
# registered. fd_poll() dispatches an event only `if (fhs && fhs->fh)` and
# otherwise ignores it — but the registration stays, and kqueue is
# level-triggered: kevent() returns that descriptor at once on every call,
# so the loop never blocks again. That is the engine thread at 100 % CPU
# while still serving every other event (audio kept flowing), until iOS
# kills the app for CPU (cpu_resource_fatal: 2026-09-12, -09-18, -09-20 —
# 48 s of CPU in 49 s, 14/16 samples in kevent under re_main). Such a
# registration is a leak by definition (fd_close() clears the handler and
# deletes the events together), so drop it where it is seen and name the
# descriptor, so the next occurrence points at whoever left it.
if ! grep -q 'Dialler: a descriptor the kqueue reports' "$SRC/src/main/main.c"; then
  perl -0pi -e 's/(\t\tif \(fhs && fhs->fh\) \{\n#if MAIN_DEBUG\n\t\t\tfd_handler\(fhs, flags\);\n#else\n\t\t\tfhs->fh\(flags, fhs->arg\);\n#endif\n\t\t\})\n\n(\t\t\/\* Handle only active events \*\/\n)/$1\n#ifdef HAVE_KQUEUE\n\t\telse if (re->method == METHOD_KQUEUE) {\n\t\t\t\/* Dialler: a descriptor the kqueue reports ready but nobody\n\t\t\t * handles (handler cleared, registration kept) is level-\n\t\t\t * triggered forever: the loop stops blocking and spins at\n\t\t\t * 100 % CPU until iOS kills the app. A leak by definition;\n\t\t\t * drop it and say which. See ios\/vendor\/patches\/apply-re.sh *\/\n\t\t\tstruct kevent kdel[2];\n\t\t\tDEBUG_WARNING("fd_poll: fd %d ready (flags %x) with no handler; dropping it from the kqueue\\n", fd, flags);\n\t\t\tEV_SET(&kdel[0], fd, EVFILT_READ,  EV_DELETE, 0, 0, 0);\n\t\t\tEV_SET(&kdel[1], fd, EVFILT_WRITE, EV_DELETE, 0, 0, 0);\n\t\t\t(void)kevent(re->kqfd, kdel, 2, NULL, 0, NULL);\n\t\t}\n#endif\n\n$2/' "$SRC/src/main/main.c"
  grep -q 'Dialler: a descriptor the kqueue reports' "$SRC/src/main/main.c" || { echo "patch: re main.c fd_poll dispatch anchor not found"; exit 1; }
fi
echo "   re: kqueue no-handler descriptor guard present"

# SIP connection lifecycle, named by its local port. A dialog is bound to the
# connection its INVITE arrived on, and the 200 OK goes back down that same
# one; when it has died in between, the answer fails with EPROTO and the call
# is lost with CallKit already showing it connected (2026-09-23, call
# ef036a07: INVITE at 11.874, answer at 15.219, "tls: SSL_write: 5"). Which
# socket that was, and when it died, is not recoverable from any existing
# log: libre says nothing when a SIP connection opens or closes, and baresip
# does not expose the dialog's transport. The local port is the join — it is
# what the server records as the registration's Contact ("…@10.18.0.204:65309")
# and what it names in its own read errors, so one capture with this on lines
# both sides up exactly.
if ! grep -q 'Dialler: SIP conn' "$SRC/src/sip/transp.c"; then
  perl -0pi -e 's/(static void conn_close\(struct sip_conn \*conn, int err\)\n\{\n\tstruct le \*le;\n\n)/$1\t\/* Dialler: SIP connection lifecycle, see ios\/vendor\/patches\/apply-re.sh *\/\n\tDEBUG_WARNING("Dialler: SIP conn closed: local=%J peer=%J %s (%m)\\n",\n\t\t      &conn->laddr, &conn->paddr, sip_transp_name(conn->tp), err);\n\n/;
    s/(\terr = tcp_conn_local_get\(conn->tc, &conn->laddr\);\n\tif \(err\)\n\t\tgoto out;\n\n)(\t\/\* Fallback check for any address win32 \*\/\n)/$1\t\/* Dialler: SIP connection lifecycle (see above) *\/\n\tDEBUG_WARNING("Dialler: SIP conn opened: local=%J peer=%J %s\\n",\n\t\t      &conn->laddr, &conn->paddr, sip_transp_name(conn->tp));\n\n$2/;' "$SRC/src/sip/transp.c"
  grep -q 'Dialler: SIP conn closed' "$SRC/src/sip/transp.c" || { echo "patch: re transp.c conn_close anchor not found"; exit 1; }
  grep -q 'Dialler: SIP conn opened' "$SRC/src/sip/transp.c" || { echo "patch: re transp.c outbound connect anchor not found"; exit 1; }
fi
echo "   re: SIP connection lifecycle logging present"

# A fatal recv() error must close the connection. tcp_recv_handler closes on
# n == 0 (orderly EOF) but on n < 0 it only logs and returns, keeping the
# descriptor registered — upstream closes on a reset only under WIN32. On
# Darwin the kqueue is level-triggered: a socket that is permanently
# errored (ENOTCONN after iOS tore the connection down under a suspended
# app) reports readable on every kevent(), the handler is re-entered at
# once, and the loop thread spins — 38 424 warnings in 1.15 s on
# 2026-09-25, each one allocating an mbuf and crossing into Swift, until the
# app's own transport reset happened to drop the socket. The only errors a
# non-blocking socket may retry are EAGAIN/EWOULDBLOCK/EINTR; anything else
# is the connection's end, and is handled exactly as EOF already is.
if ! grep -q 'Dialler: fatal recv' "$SRC/src/tcp/tcp.c"; then
  perl -0pi -e 's/(\t\tDEBUG_WARNING\("recv handler: recv\(\): %m\\n", err\);\n)(#ifdef WIN32\n)/$1#ifndef WIN32\n\t\t\/* Dialler: fatal recv error closes the connection, as EOF does;\n\t\t * see ios\/vendor\/patches\/apply-re.sh *\/\n\t\tif (err != EAGAIN && err != EWOULDBLOCK && err != EINTR) {\n\t\t\tmem_deref(mb);\n\t\t\tconn_close(tc, err);\n\t\t\treturn;\n\t\t}\n#endif\n$2/' "$SRC/src/tcp/tcp.c"
  grep -q 'Dialler: fatal recv' "$SRC/src/tcp/tcp.c" || { echo "patch: re tcp.c recv error anchor not found"; exit 1; }
fi
echo "   re: fatal recv error closes the connection"

# A dead UDP descriptor must leave the poll set. udp_read() returns on any
# recv error and keeps the descriptor registered; on Darwin the kqueue is
# level-triggered, so a socket that is permanently errored — one iOS
# defuncted under a suspended app, ENOTCONN for ever — is reported readable
# on every kevent(), udp_read() runs again at once (mbuf_alloc, recvfrom,
# mem_deref, nothing else), and the loop never blocks: the engine thread at
# 99 % CPU until iOS kills the app for it (cpu_resource_fatal 2026-09-27
# 12:49 and 12:56, 48 s of CPU in 49 s, both mid-call in the background;
# every one of that day's 13 runs that spun had been suspended first, and
# none that had not). The TCP twin of this is patch level 6. The sockets
# that live across a suspension are the DNS client's two (dnsc_alloc), so
# that client also reopens a socket it is told has died.
if ! grep -q 'Dialler: udp fd' "$SRC/src/udp/udp.c"; then
  perl -0pi -e 's{(\tbool conn;[^\n]*\n)}{$1\tunsigned rxerrs;     /**< Dialler: consecutive recv errors */\n};
    s{(static void udp_read\(struct udp_sock \*us, re_sock_t fd\)\n\{)}{/* Dialler: is this receive error the end of the descriptor? ENOTCONN is what\n * a socket iOS defuncted under a suspended app returns for ever; EBADF,\n * ENOTSOCK and EPIPE likewise never clear. Any other error is transient (an\n * ICMP for a connected socket, say) and is reported as before; a streak of\n * them with no datagram between is announced once, so an unknown permanent\n * errno names itself next time. See ios/vendor/patches/apply-re.sh */\nstatic bool udp_rx_dead(struct udp_sock *us, int err)\n{\n\tswitch (err) {\n\n\tcase ENOTCONN:\n\tcase EBADF:\n\tcase ENOTSOCK:\n\tcase EPIPE:\n\t\treturn true;\n\n\tdefault:\n\t\tif (++us->rxerrs == 64)\n\t\t\tDEBUG_WARNING("Dialler: udp fd %d: 64 consecutive recv errors (%m)\\n", us->fd, err);\n\t\treturn false;\n\t}\n}\n\n\n$1};
    s{(\t\tif \(us->eh\)\n\t\t\tus->eh\(err, us->arg\);\n\n\t\tgoto out;\n\t\}\n\n)(\tmb->pos = us->rx_presz;\n)}{\t\t/* Dialler: a dead descriptor leaves the poll set now, before the\n\t\t * owner hears of it (it may free us); kqueue would otherwise report\n\t\t * it readable on every call and the loop would never block again. */\n\t\tif (udp_rx_dead(us, err)) {\n\t\t\tDEBUG_WARNING("Dialler: udp fd %d dead (%m); dropping it from the poll set\\n", fd, err);\n\t\t\tus->fhs = fd_close(us->fhs);\n\t\t}\n\n$1\tus->rxerrs = 0;\n$2};' "$SRC/src/udp/udp.c"
  grep -q 'unsigned rxerrs' "$SRC/src/udp/udp.c" || { echo "patch: re udp.c struct anchor not found"; exit 1; }
  grep -q 'static bool udp_rx_dead' "$SRC/src/udp/udp.c" || { echo "patch: re udp.c udp_read anchor not found"; exit 1; }
  grep -q 'Dialler: udp fd %d dead' "$SRC/src/udp/udp.c" || { echo "patch: re udp.c error path anchor not found"; exit 1; }
fi
if ! grep -q 'Dialler: dns client socket' "$SRC/src/dns/client.c"; then
  perl -0pi -e 's{(^int dnsc_alloc\(struct dnsc \*\*dcpp, const struct dnsc_conf \*conf,)}{/* Dialler: the client keeps its two sockets for the life of the stack. One\n * iOS defuncted under a suspended app is reported dead by udp_read() (patch\n * level 7) and dropped from the poll set; reopen it here so name resolution\n * outlives a suspension. See ios/vendor/patches/apply-re.sh */\nstatic void udp_error_handler4(int err, void *arg);\nstatic void udp_error_handler6(int err, void *arg);\n\n\nstatic void dns_udp_reopen(struct dnsc *dnsc, struct udp_sock **usp, int af,\n\t\t\t   int err)\n{\n\tstruct sa laddr;\n\tint e;\n\n\tif (err != ENOTCONN && err != EBADF && err != ENOTSOCK && err != EPIPE)\n\t\treturn;\n\n\t*usp = mem_deref(*usp);\n\tsa_set_str(&laddr, af == AF_INET6 ? "::" : "0.0.0.0", 0);\n\te = udp_listen(usp, &laddr, udp_recv_handler, dnsc);\n\tif (!e)\n\t\tudp_error_handler_set(*usp, af == AF_INET6 ? udp_error_handler6\n\t\t\t\t\t\t\t: udp_error_handler4);\n\n\tif (e)\n\t\tDEBUG_WARNING("Dialler: dns client socket (af %d) died (%m); reopen failed (%m)\\n",\n\t\t\t      af, err, e);\n\telse\n\t\tDEBUG_WARNING("Dialler: dns client socket (af %d) died (%m); reopened\\n",\n\t\t\t      af, err);\n}\n\n\nstatic void udp_error_handler4(int err, void *arg)\n{\n\tstruct dnsc *dnsc = arg;\n\n\tdns_udp_reopen(dnsc, &dnsc->us, AF_INET, err);\n}\n\n\nstatic void udp_error_handler6(int err, void *arg)\n{\n\tstruct dnsc *dnsc = arg;\n\n\tdns_udp_reopen(dnsc, &dnsc->us6, AF_INET6, err);\n}\n\n\n$1}m;
    s{(\terr &= udp_listen\(&dnsc->us6, &laddr6, udp_recv_handler, dnsc\);\n)}{$1\tudp_error_handler_set(dnsc->us, udp_error_handler4);  /* Dialler */\n\tudp_error_handler_set(dnsc->us6, udp_error_handler6); /* Dialler */\n};' "$SRC/src/dns/client.c"
  grep -q 'static void dns_udp_reopen' "$SRC/src/dns/client.c" || { echo "patch: re client.c dnsc_alloc anchor not found"; exit 1; }
  grep -q 'udp_error_handler_set(dnsc->us6' "$SRC/src/dns/client.c" || { echo "patch: re client.c socket anchor not found"; exit 1; }
fi
echo "   re: dead UDP descriptors leave the poll set; DNS client reopens its sockets"

# ---- patch level ------------------------------------------------------------
# Exported so the app can log which libre it was linked against; bump when a
# patch above changes. cb_version() prints it as "libre patch level N".
#   1: re_main EBADF guard   2: + Apple SO_NET_SERVICE_TYPE / IPV6_TCLASS marks
#   3: + sip_transp_flush closes connections before dropping them
#   4: + fd_poll drops a ready kqueue descriptor that has no handler
#   5: + SIP connections say when they open and close, by local port
#   6: + a fatal recv() error closes the TCP connection instead of spinning
#   7: + a dead UDP descriptor leaves the poll set; the DNS client reopens its sockets
RE_PATCH_LEVEL=7
if ! grep -q 're_dialler_patchlevel' "$SRC/src/main/main.c"; then
  printf '\n/* Dialler: patch level, see ios/vendor/patches/apply-re.sh */\nint re_dialler_patchlevel(void)\n{\n\treturn %s;\n}\n' "$RE_PATCH_LEVEL" >> "$SRC/src/main/main.c"
fi
# -0: the pattern spans lines. Without it (as it was until level 3) the
# bump silently never matched and the app kept reporting the old level
# while carrying the new code (2026-09-18).
grep -q "return $RE_PATCH_LEVEL;" "$SRC/src/main/main.c" || perl -0pi -e 's/(int re_dialler_patchlevel\(void\)\n\{\n\treturn )\d+;/${1}'"$RE_PATCH_LEVEL"';/' "$SRC/src/main/main.c"
grep -q "return $RE_PATCH_LEVEL;" "$SRC/src/main/main.c" || { echo "patch: re patch level not updated"; exit 1; }
echo "   re: patch level $RE_PATCH_LEVEL"
