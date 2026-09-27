/* Does libre's loop stop spinning on a UDP descriptor that is dead?
 *
 * On iOS a suspended app's sockets are defuncted: every read fails at once
 * with a permanent error (ENOTCONN) and kqueue, level-triggered, reports the
 * descriptor readable on every call. Upstream udp_read() returns on the
 * error and leaves the descriptor registered, so re_main() never blocks
 * again: the engine thread at 99 % CPU until iOS kills the app
 * (cpu_resource_fatal, 48 s in 49 s, 2026-09-12 → 2026-09-27; the DNS
 * client's two sockets are the ones that live across a suspension).
 *
 * The sandbox refuses bind() and SO_DEFUNCTIT, so the dead descriptor is a
 * pipe whose writer is closed, dup2'd over the UDP socket's number: kqueue
 * says readable for ever and recvfrom() fails for ever with ENOTSOCK — the
 * same shape. The loop runs for one second; with libre patch level 7 the
 * descriptor is dropped on the first error and the loop sleeps (CPU well
 * under 0.2 s); without it the loop burns the whole second.
 *
 *   make re-udp-dead-probe     (macOS slice of libre; PASS/FAIL)
 */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <unistd.h>

#include <re.h>

int re_dialler_patchlevel(void); /* appended to libre main.c by apply-re.sh */

static unsigned g_errors;
static int g_last_err;
static struct tmr g_stop;

static void stop(void *arg)
{
	(void)arg;
	re_cancel();
}

static void recv_handler(const struct sa *src, struct mbuf *mb, void *arg)
{
	(void)src; (void)mb; (void)arg;
}

static void error_handler(int err, void *arg)
{
	(void)arg;
	++g_errors;
	g_last_err = err;
}

static double cpu_seconds(void)
{
	struct rusage ru;
	getrusage(RUSAGE_SELF, &ru);
	return ru.ru_utime.tv_sec + ru.ru_utime.tv_usec / 1e6 +
	       ru.ru_stime.tv_sec + ru.ru_stime.tv_usec / 1e6;
}

int main(void)
{
	struct udp_sock *us = NULL;
	int p[2];
	int fd, err;
	double cpu0, cpu;

	err = libre_init();
	if (err) { fprintf(stderr, "libre_init: %s\n", strerror(err)); return 2; }

	err = udp_open(&us, AF_INET);
	if (err) { fprintf(stderr, "udp_open: %s\n", strerror(err)); return 2; }
	udp_handler_set(us, recv_handler, NULL);
	udp_error_handler_set(us, error_handler);
	fd = udp_sock_fd(us, AF_INET);

	/* Make the descriptor dead: a pipe with no writer, on the socket's number. */
	if (pipe(p)) { perror("pipe"); return 2; }
	close(p[1]);
	if (dup2(p[0], fd) < 0) { perror("dup2"); return 2; }
	close(p[0]);
	fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK);

	err = udp_thread_attach(us); /* fd_listen on the dead descriptor */
	if (err) { fprintf(stderr, "udp_thread_attach: %s\n", strerror(err)); return 2; }

	tmr_init(&g_stop);
	tmr_start(&g_stop, 1000, stop, NULL);

	cpu0 = cpu_seconds();
	err = re_main(NULL);
	cpu = cpu_seconds() - cpu0;

	printf("re_main returned %d after 1 s; loop CPU %.3f s; error handler ran %u time(s), last %s (%d); libre patch level %d\n",
	       err, cpu, g_errors, g_last_err ? strerror(g_last_err) : "-", g_last_err, re_dialler_patchlevel());

	tmr_cancel(&g_stop);
	mem_deref(us);
	libre_close();

	if (cpu > 0.2) {
		printf("FAIL: the loop spun on a dead UDP descriptor\n");
		return 1;
	}
	if (g_errors == 0) {
		printf("FAIL: the owner was never told the descriptor died\n");
		return 1;
	}
	printf("PASS: dead UDP descriptor dropped from the poll set; owner told; loop idle\n");
	return 0;
}
