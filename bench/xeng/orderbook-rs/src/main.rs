// Replay adapter for OrderBook-rs in the cross-engine comparison.
// Reads a tape from stdin, replays it into a fresh book timing only the
// replay loop, then prints per-command results, trades and the terminal book.

use std::io::{self, BufWriter, Read, Write};
use std::process::ExitCode;
use std::time::Instant;

use orderbook_rs::{Id, OrderBook, Side, TimeInForce};
use pricelevel::Hash32;

enum Cmd {
    Submit { acct: u64, side: Side, price: u128, qty: u64 },
    Cancel { target: u64 },
}

struct Fill {
    price: u128,
    qty: u64,
    maker: u64,
    taker: u64,
    aggressor: u8,
}

fn parse(input: &str) -> Result<Vec<Cmd>, String> {
    let mut lines = input.lines().filter(|l| !l.trim().is_empty());
    let header = lines.next().ok_or("empty input")?;
    let mut h = header.split_ascii_whitespace();
    if h.next() != Some("N") {
        return Err(format!("bad header: {header}"));
    }
    let n: usize = h.next().and_then(|s| s.parse().ok()).ok_or("bad N")?;

    let mut cmds = Vec::with_capacity(n);
    for line in lines {
        let f: Vec<&str> = line.split_ascii_whitespace().collect();
        let num = |i: usize| -> Result<i64, String> {
            f.get(i)
                .and_then(|s| s.parse().ok())
                .ok_or_else(|| format!("bad field {i}: {line}"))
        };
        let pos = num(1)?;
        if pos != cmds.len() as i64 {
            return Err(format!("position {pos} out of sequence"));
        }
        let cmd = match f.first() {
            Some(&"S") => Cmd::Submit {
                acct: num(2)? as u64,
                side: if num(3)? == 0 { Side::Buy } else { Side::Sell },
                price: u128::try_from(num(4)?).map_err(|_| "negative price")?,
                qty: u64::try_from(num(5)?).map_err(|_| "negative qty")?,
            },
            Some(&"C") => Cmd::Cancel { target: num(3)? as u64 },
            _ => return Err(format!("bad line: {line}")),
        };
        cmds.push(cmd);
    }
    if cmds.len() != n {
        return Err(format!("N says {n}, got {} commands", cmds.len()));
    }
    Ok(cmds)
}

// Distinct non-zero owner per account. STP is off, so this only keys the
// engine's per-user order index.
fn owner(acct: u64) -> Hash32 {
    let mut b = [0u8; 32];
    b[..8].copy_from_slice(&(acct + 1).to_le_bytes());
    Hash32(b)
}

fn pos_of(id: Id) -> u64 {
    match id {
        Id::Sequential(n) => n,
        _ => u64::MAX,
    }
}

fn main() -> ExitCode {
    let mut input = String::new();
    if let Err(e) = io::stdin().read_to_string(&mut input) {
        eprintln!("read stdin: {e}");
        return ExitCode::FAILURE;
    }
    let cmds = match parse(&input) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("parse: {e}");
            return ExitCode::FAILURE;
        }
    };
    drop(input);

    // A submit of qty <= 9 can produce at most 9 trades.
    let max_fills: usize = cmds
        .iter()
        .map(|c| match c {
            Cmd::Submit { qty, .. } => *qty as usize,
            Cmd::Cancel { .. } => 0,
        })
        .sum();
    let mut refused = vec![0u8; cmds.len()];
    let mut fill_end = vec![0u32; cmds.len()];
    let mut fills: Vec<Fill> = Vec::with_capacity(max_fills);
    let mut errors = 0u64;

    // Default book: no STP, no tick/lot size, no fees, no risk, no listeners.
    let book: OrderBook<()> = OrderBook::new("XENG");

    let start = Instant::now();
    for (pos, cmd) in cmds.iter().enumerate() {
        match *cmd {
            Cmd::Submit { acct, side, price, qty } => {
                match book.add_limit_order_with_user_and_result(
                    Id::Sequential(pos as u64),
                    price,
                    qty,
                    side,
                    TimeInForce::Gtc,
                    owner(acct),
                    None,
                ) {
                    Ok((_, Some(tr))) => {
                        for t in tr.match_result.trades().as_vec() {
                            fills.push(Fill {
                                price: t.price().as_u128(),
                                qty: t.quantity().as_u64(),
                                maker: pos_of(t.maker_order_id()),
                                taker: pos_of(t.taker_order_id()),
                                aggressor: if t.taker_side() == Side::Buy { b'B' } else { b'S' },
                            });
                        }
                    }
                    Ok((_, None)) => {}
                    Err(_) => {
                        refused[pos] = 1;
                        errors += 1;
                    }
                }
            }
            Cmd::Cancel { target } => match book.cancel_order(Id::Sequential(target)) {
                Ok(Some(_)) => {}
                Ok(None) => refused[pos] = 1,
                Err(_) => {
                    refused[pos] = 1;
                    errors += 1;
                }
            },
        }
        fill_end[pos] = fills.len() as u32;
    }
    let replay_ns = start.elapsed().as_nanos();

    if errors > 0 {
        eprintln!("engine returned {errors} errors");
    }

    let stdout = io::stdout();
    let mut out = BufWriter::with_capacity(1 << 20, stdout.lock());
    if let Err(e) = report(&mut out, &book, &cmds, &refused, &fill_end, &fills, replay_ns) {
        eprintln!("write: {e}");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

fn report(
    out: &mut impl Write,
    book: &OrderBook<()>,
    cmds: &[Cmd],
    refused: &[u8],
    fill_end: &[u32],
    fills: &[Fill],
    replay_ns: u128,
) -> io::Result<()> {
    let mut i = 0usize;
    for (pos, &end) in fill_end.iter().enumerate() {
        writeln!(out, "c {pos} {}", refused[pos])?;
        for f in &fills[i..end as usize] {
            writeln!(
                out,
                "X {} {} {} {} {}",
                f.price, f.qty, f.maker, f.taker, f.aggressor as char
            )?;
        }
        i = end as usize;
    }

    // Terminal book from the engine: bids best-first, asks best-first, each
    // level in insertion-sequence order (the order the matcher consumes).
    // Resting orders carry only their remaining quantity, so the original
    // quantity comes from the submit that made the order.
    let snap = book
        .create_snapshot(usize::MAX)
        .map_err(|e| io::Error::other(format!("snapshot: {e}")))?;
    for (side, levels) in [(0, &snap.bids), (1, &snap.asks)] {
        for level in levels {
            for o in level.orders() {
                let pos = pos_of(o.id());
                let orig = match cmds.get(pos as usize) {
                    Some(Cmd::Submit { qty, .. }) => *qty,
                    _ => 0,
                };
                let remaining = o.visible_quantity().as_u64() + o.hidden_quantity().as_u64();
                writeln!(
                    out,
                    "L {pos} {side} {} {orig} {}",
                    o.price().as_u128(),
                    orig.saturating_sub(remaining)
                )?;
            }
        }
    }

    writeln!(out, "E {} {}", book.last_trade_price().unwrap_or(0), fills.len())?;
    writeln!(out, "T {replay_ns}")?;
    out.flush()
}
