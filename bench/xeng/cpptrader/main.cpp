// cpptrader-xeng replays a basic cross-engine tape through CppTrader's
// Matching::MarketManager and prints what the engine did, in the line protocol
// internal/benchgate/xeng.go reads back.
//
// The engine names orders by uint64 id and treats id 0 as invalid (it is also the
// empty key of its order hash map). The order made at tape position pos gets id
// pos+1, so a cancel naming a position that made no order names an id the engine
// never saw.

#include "trader/matching/market_manager.h"

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <iterator>
#include <string>
#include <vector>

#ifndef NDEBUG
#error "build with NDEBUG: the engine asserts on a cancel of an unknown order"
#endif

using namespace CppTrader::Matching;

namespace {

struct Command {
    bool cancel;
    bool sell;
    int64_t target; // cancels: the position whose order is cancelled
    uint64_t price;
    uint64_t qty;
};

struct Trade {
    uint64_t price, qty, maker, taker;
};

[[noreturn]] void fail(const std::string& msg)
{
    std::fprintf(stderr, "cpptrader: %s\n", msg.c_str());
    std::exit(1);
}

// Recorder turns the engine's execution callbacks into trades. For a limit order
// the engine reports each fill as two onExecuteOrder calls: first the resting
// order it executes against, then the incoming order, both with the resting
// order's price and the fill quantity. Anything else (two resting orders in a row,
// a mismatched pair, an unpaired call) is matching outside that path and is
// flagged rather than guessed at.
class Recorder : public MarketHandler {
public:
    std::vector<Trade> trades;
    uint64_t taker = 0; // id of the order the current command submits, 0 for cancels
    bool broken = false;

    // Called after each command: a maker call left waiting means a fill with no taker.
    void endCommand()
    {
        if (_maker != 0)
            broken = true;
        _maker = 0;
    }

protected:
    void onExecuteOrder(const Order& order, uint64_t price, uint64_t quantity) override
    {
        if (taker == 0 || order.Id != taker) {
            if (_maker != 0)
                broken = true;
            _maker = order.Id;
            _price = price;
            _qty = quantity;
            return;
        }
        if (_maker == 0 || price != _price || quantity != _qty)
            broken = true;
        trades.push_back(Trade{price, quantity, _maker, taker});
        _maker = 0;
    }

private:
    uint64_t _maker = 0, _price = 0, _qty = 0;
};

// parse reads the whole tape. It returns the commands indexed by position and
// counts the submits.
std::vector<Command> parse(const std::string& in, size_t& submits)
{
    std::vector<Command> cmds;
    bool seenN = false;
    submits = 0;
    size_t ln = 0;
    const char* p = in.data();
    const char* end = p + in.size();
    while (p < end) {
        const char* eol = static_cast<const char*>(std::memchr(p, '\n', end - p));
        if (eol == nullptr)
            eol = end;
        std::string line(p, eol);
        p = eol + 1;
        ++ln;

        char kind = 0;
        long long v[5];
        int n = 0, used = 0;
        if (std::sscanf(line.c_str(), " %c%n", &kind, &used) != 1)
            continue; // blank line
        const char* q = line.c_str() + used;
        for (int got; n < 5 && std::sscanf(q, "%lld%n", &v[n], &got) == 1; ++n)
            q += got;
        int rest = 0;
        if (std::sscanf(q, " %*c%n", &rest) != EOF && rest > 0)
            fail("line " + std::to_string(ln) + ": trailing fields");

        auto bad = [&](const char* what) { fail("line " + std::to_string(ln) + ": " + what); };
        if (kind == 'N' && n == 1 && v[0] >= 0 && !seenN) {
            cmds.reserve(static_cast<size_t>(v[0]));
            seenN = true;
        } else if (kind == 'S' && n == 5 && seenN) {
            // S pos acct side price qty
            if (v[0] != static_cast<long long>(cmds.size()) || v[2] < 0 || v[2] > 1 || v[3] <= 0 || v[4] <= 0)
                bad("bad submit");
            cmds.push_back(Command{false, v[2] == 1, 0, static_cast<uint64_t>(v[3]), static_cast<uint64_t>(v[4])});
            ++submits;
        } else if (kind == 'C' && n == 3 && seenN) {
            // C pos acct target
            if (v[0] != static_cast<long long>(cmds.size()))
                bad("bad cancel");
            cmds.push_back(Command{true, false, v[2], 0, 0});
        } else {
            bad("not a tape line");
        }
    }
    if (!seenN || cmds.size() != cmds.capacity())
        fail("tape: header count does not match " + std::to_string(cmds.size()) + " commands");
    return cmds;
}

// pos maps an engine order id back to the tape position that made it.
size_t pos(uint64_t id, size_t n)
{
    if (id == 0 || id > n)
        fail("engine reported unknown order id " + std::to_string(id));
    return static_cast<size_t>(id - 1);
}

} // namespace

int main()
{
    std::ios::sync_with_stdio(false);
    std::string in((std::istreambuf_iterator<char>(std::cin)), std::istreambuf_iterator<char>());
    if (std::cin.bad())
        fail("read stdin");
    size_t submits = 0;
    std::vector<Command> cmds = parse(in, submits);
    const size_t n = cmds.size();

    Recorder rec;
    // A trade either fills its maker or ends its taker's command, so there are at
    // most submits+n of them.
    rec.trades.reserve(submits + n);
    std::vector<uint8_t> refused(n, 0);
    std::vector<size_t> tradeEnd(n, 0);

    MarketManager market(rec);
    char name[8] = {'X', 'E', 'N', 'G', 0, 0, 0, 0};
    Symbol symbol(0, name);
    if (market.AddSymbol(symbol) != ErrorCode::OK || market.AddOrderBook(symbol) != ErrorCode::OK)
        fail("cannot create the symbol and its order book");
    // Matching is off by default; enabled, every add matches before it rests.
    market.EnableMatching();

    auto start = std::chrono::steady_clock::now();
    for (size_t i = 0; i < n; ++i) {
        const Command& c = cmds[i];
        ErrorCode r;
        if (c.cancel) {
            rec.taker = 0;
            r = (c.target < 0) ? ErrorCode::ORDER_NOT_FOUND : market.DeleteOrder(static_cast<uint64_t>(c.target) + 1);
        } else {
            rec.taker = i + 1;
            r = market.AddOrder(Order::Limit(i + 1, 0, c.sell ? OrderSide::SELL : OrderSide::BUY, c.price, c.qty));
        }
        rec.endCommand();
        refused[i] = (r != ErrorCode::OK);
        tradeEnd[i] = rec.trades.size();
    }
    auto replayNS = std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - start).count();

    if (rec.broken)
        fail("engine reported executions that do not pair as resting order then incoming order");

    static char obuf[1 << 20];
    std::setvbuf(stdout, obuf, _IOFBF, sizeof(obuf));
    size_t t = 0;
    for (size_t i = 0; i < n; ++i) {
        std::printf("c %zu %d\n", i, refused[i]);
        for (; t < tradeEnd[i]; ++t) {
            const Trade& tr = rec.trades[t];
            size_t maker = pos(tr.maker, n);
            size_t taker = pos(tr.taker, n);
            if (taker != i || cmds[maker].cancel || cmds[maker].sell == cmds[taker].sell)
                fail("trade " + std::to_string(t) + " does not fit its command");
            std::printf("X %llu %llu %zu %zu %c\n", static_cast<unsigned long long>(tr.price),
                static_cast<unsigned long long>(tr.qty), maker, taker, cmds[taker].sell ? 'S' : 'B');
        }
    }

    // The book is read from the engine. Both level trees are ascending by price, so
    // bids are walked from the top; each level's order list is in time priority.
    const OrderBook* book = market.GetOrderBook(0);
    if (book == nullptr)
        fail("order book vanished");
    std::vector<const LevelNode*> bids, asks;
    for (const auto& level : book->bids())
        bids.push_back(&level);
    for (const auto& level : book->asks())
        asks.push_back(&level);
    std::vector<const LevelNode*> walk(bids.rbegin(), bids.rend());
    walk.insert(walk.end(), asks.begin(), asks.end());
    for (const LevelNode* level : walk) {
        uint64_t volume = 0;
        for (const auto& o : level->OrderList) {
            size_t p = pos(o.Id, n);
            const Command& c = cmds[p];
            if (c.cancel || o.Price != level->Price || o.Quantity != c.qty || o.ExecutedQuantity + o.LeavesQuantity != o.Quantity || o.IsSell() != c.sell)
                fail("resting order " + std::to_string(p) + " disagrees with its submit");
            volume += o.LeavesQuantity;
            std::printf("L %zu %d %llu %llu %llu\n", p, o.IsSell() ? 1 : 0, static_cast<unsigned long long>(o.Price),
                static_cast<unsigned long long>(o.Quantity), static_cast<unsigned long long>(o.ExecutedQuantity));
        }
        if (volume != level->TotalVolume)
            fail("level " + std::to_string(level->Price) + " volume disagrees with its orders");
    }

    uint64_t last = rec.trades.empty() ? 0 : rec.trades.back().price;
    std::printf("E %llu %zu\n", static_cast<unsigned long long>(last), rec.trades.size());
    std::printf("T %lld\n", static_cast<long long>(replayNS));
    if (std::fflush(stdout) != 0)
        fail("write stdout");
    return 0;
}
