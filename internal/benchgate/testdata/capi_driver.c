/*
 * capi_driver replays a tape through libobook's C ABI (docs/C-API.md) and prints
 * what came back, so a Go test can fold it into the OBDG digest and compare with the
 * native engine. It deliberately knows nothing about OBDG: it only calls the ABI and
 * prints integers.
 *
 * Input, one command per line, after a first line "N <count>":
 *   S <pos> <user> <side> <type> <price> <qty> <tif> <postonly>
 *   C <pos> <user> <target>
 *   R <pos> <user> <target> <newqty>
 *   P <pos> <user> <target> <side> <type> <price> <qty> <tif> <postonly>
 *
 * Output: per command "C <pos> <status> <reason>", then its events as
 * "E <kind> <reason> <aggressor> <order_id> <user> <price> <qty> <maker> <taker>";
 * at the end the resting book as "L <order_id> <user> <side> <price> <qty> <filled>"
 * and "T <last_trade_price>". A call that returns -1 prints "X <pos>" and exits 1.
 */
#include <stdio.h>
#include <stdlib.h>
#include "libobook.h"

static void drain(int64_t h) {
	ob_event ev[64];
	int32_t n;
	while ((n = ob_events(h, ev, 64)) > 0) {
		for (int32_t i = 0; i < n; i++) {
			printf("E %d %d %d %lld %lld %lld %lld %lld %lld\n", ev[i].kind, ev[i].reason, ev[i].aggressor,
			       (long long)ev[i].order_id, (long long)ev[i].user, (long long)ev[i].price,
			       (long long)ev[i].qty, (long long)ev[i].maker_id, (long long)ev[i].taker_id);
		}
	}
	if (n < 0) {
		printf("X events\n");
		exit(1);
	}
}

int main(void) {
	if (ob_abi_version() != 1) {
		fprintf(stderr, "ABI version %d, want 1\n", ob_abi_version());
		return 1;
	}
	long long count;
	if (scanf(" N %lld", &count) != 1) return 1;
	int64_t *ids = calloc((size_t)count, sizeof(int64_t));
	int64_t h = ob_new(1000000);
	char k;
	long long pos, user, side, type, price, qty, tif, po, target, newqty;
	while (scanf(" %c %lld %lld", &k, &pos, &user) == 3) {
		int32_t status = 0, reason = 0, rc = -1;
		int64_t id = 0;
		switch (k) {
		case 'S':
			scanf("%lld %lld %lld %lld %lld %lld", &side, &type, &price, &qty, &tif, &po);
			rc = ob_submit(h, user, (int32_t)side, (int32_t)type, price, qty, (int32_t)tif, (int32_t)po,
			               &id, &status, &reason);
			ids[pos] = id;
			break;
		case 'C':
			scanf("%lld", &target);
			rc = ob_cancel(h, ids[target], user, &status, &reason);
			break;
		case 'R':
			scanf("%lld %lld", &target, &newqty);
			rc = ob_reduce(h, ids[target], newqty, user, &status, &reason);
			break;
		case 'P':
			scanf("%lld %lld %lld %lld %lld %lld %lld", &target, &side, &type, &price, &qty, &tif, &po);
			rc = ob_replace(h, ids[target], user, (int32_t)side, (int32_t)type, price, qty, (int32_t)tif,
			                (int32_t)po, &id, &status, &reason);
			ids[pos] = id;
			break;
		}
		if (rc != 0) {
			printf("X %lld\n", pos);
			return 1;
		}
		printf("C %lld %d %d\n", pos, status, reason);
		drain(h);
	}
	static ob_order book[1000000];
	int32_t n = ob_book(h, book, 1000000);
	if (n < 0) {
		printf("X book\n");
		return 1;
	}
	for (int32_t i = 0; i < n; i++) {
		printf("L %lld %lld %d %lld %lld %lld\n", (long long)book[i].order_id, (long long)book[i].user,
		       book[i].side, (long long)book[i].price, (long long)book[i].qty, (long long)book[i].filled);
	}
	printf("T %lld\n", (long long)ob_last_trade_price(h));
	ob_free(h);
	free(ids);
	return 0;
}
