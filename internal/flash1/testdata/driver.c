// A stand-in for the flash1 harness, written here from the header's documented layout:
// it owns a transport, drives the adapter's exports, and prints every report. Input,
// one per line:
//
//	B n            the next n messages go in one engine_on_batch call
//	S              the next message goes through its own engine_on_* call
//	N oid seq price qty side ioc | C oid seq | M oid seq price qty side
//	Q price side   print best bid, best ask and the depth at (price, side)
//
// The transport refuses every fifth push, so the adapter's retry is exercised.
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef struct { uint64_t order_id, seq; int64_t price; uint32_t qty; uint8_t side, ioc, r[2]; } new_order_t;
typedef struct { uint64_t order_id, seq; } cancel_t;
typedef struct { uint64_t order_id, seq; int64_t price; uint32_t qty; uint8_t side, r[3]; } modify_t;
typedef struct { uint8_t tag, pad[7]; union { new_order_t n; cancel_t c; modify_t m; } u; } me_msg_t;
typedef struct {
	uint8_t type, side, r[6];
	uint64_t seq, order_id;
	int64_t price;
	uint32_t qty, r2;
	uint64_t maker, taker, r3;
} me_report_t;
typedef struct {
	void *(*create)(uint32_t);
	int (*push)(void *, const me_report_t *);
	uint32_t (*drain)(void *, me_report_t *, uint32_t);
	void (*flush)(void *);
	void (*destroy)(void *);
} me_transport_t;

_Static_assert(sizeof(me_msg_t) == 40, "me_msg_t");
_Static_assert(sizeof(me_report_t) == 64, "me_report_t");

void engine_init(uint64_t, me_transport_t *, void *);
void engine_shutdown(void);
void engine_flush(void);
void engine_on_new_order(new_order_t *);
void engine_on_cancel(cancel_t *);
void engine_on_modify(modify_t *);
void engine_on_batch(me_msg_t *, uint32_t);
int64_t engine_query_best_bid(void);
int64_t engine_query_best_ask(void);
uint64_t engine_query_depth_at(int64_t, uint8_t);

static unsigned long attempts;

static int push(void *sink, const me_report_t *r) {
	(void)sink;
	if (++attempts % 5 == 0) return 0;
	printf("R %u %u %llu %llu %lld %u %llu %llu\n", r->type, r->side, (unsigned long long)r->seq,
	       (unsigned long long)r->order_id, (long long)r->price, r->qty, (unsigned long long)r->maker,
	       (unsigned long long)r->taker);
	return 1;
}

static int readmsg(me_msg_t *m) {
	char k;
	unsigned long long a, b;
	long long p;
	unsigned q, s, i;
	memset(m, 0, sizeof *m);
	if (scanf(" %c", &k) != 1) return 0;
	switch (k) {
	case 'N':
		if (scanf("%llu %llu %lld %u %u %u", &a, &b, &p, &q, &s, &i) != 6) return 0;
		m->tag = 0;
		m->u.n = (new_order_t){.order_id = a, .seq = b, .price = p, .qty = q, .side = s, .ioc = i};
		return 1;
	case 'C':
		if (scanf("%llu %llu", &a, &b) != 2) return 0;
		m->tag = 1;
		m->u.c = (cancel_t){.order_id = a, .seq = b};
		return 1;
	case 'M':
		if (scanf("%llu %llu %lld %u %u", &a, &b, &p, &q, &s) != 5) return 0;
		m->tag = 2;
		m->u.m = (modify_t){.order_id = a, .seq = b, .price = p, .qty = q, .side = s};
		return 1;
	}
	return 0;
}

static me_msg_t batch[4096];

int main(void) {
	me_transport_t t = {.push = push};
	engine_init(1, &t, NULL);
	char k;
	while (scanf(" %c", &k) == 1) {
		if (k == 'B') {
			unsigned n;
			if (scanf("%u", &n) != 1 || n > 4096) return 2;
			for (unsigned i = 0; i < n; i++)
				if (!readmsg(&batch[i])) return 3;
			engine_on_batch(batch, n);
		} else if (k == 'S') {
			me_msg_t m;
			if (!readmsg(&m)) return 3;
			if (m.tag == 0) engine_on_new_order(&m.u.n);
			else if (m.tag == 1) engine_on_cancel(&m.u.c);
			else engine_on_modify(&m.u.m);
		} else if (k == 'Q') {
			long long p;
			unsigned s;
			if (scanf("%lld %u", &p, &s) != 2) return 3;
			printf("Q %lld %lld %llu\n", (long long)engine_query_best_bid(), (long long)engine_query_best_ask(),
			       (unsigned long long)engine_query_depth_at(p, (uint8_t)s));
		} else {
			return 4;
		}
	}
	engine_flush();
	engine_shutdown();
	return 0;
}
