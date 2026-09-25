//! The three benchmarks, over whichever HTTP/2 client implements Conn.
//!
//! A connection is one HTTP/2 connection to one echo URL, which every request
//! sent on it is multiplexed over, each on a stream of its own: `-c` of them,
//! spread over the framework's fifty ports. The client keeps to the server's
//! SETTINGS_MAX_CONCURRENT_STREAMS by itself, holding a request back until a
//! stream is free rather than having it refused.

use bytes::Bytes;
use std::collections::{BTreeMap, VecDeque};
use std::future::Future;
use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::calc::{self, Stats};

/// The client's receive windows: large enough that flow control never holds
/// a server back, which is a property of the client and not of the server
/// being measured. The Go client's are the same.
pub const STREAM_WINDOW: u32 = 1 << 30;
pub const CONN_WINDOW: u32 = 1 << 30;

/// One HTTP/2 connection of a client, in cleartext with prior knowledge,
/// which is all the servers speak on their benchmark ports.
pub trait Conn: Sized + Send + Sync + 'static {
    /// The report's BenchClient: the directory the client is built from.
    const BENCH_CLIENT: &'static str;
    /// How the Summary and the console show the client, as report.clientName
    /// does: language-framework.
    const NAME: &'static str;

    /// Opens the connection to url, and has one GET of it answered with a
    /// 200 before it counts, since a connection the kernel accepted is not
    /// yet one the server is serving. timeout bounds both.
    fn dial(url: &str, timeout: Duration) -> impl Future<Output = Result<Self, String>> + Send;

    /// A POST of body to the echo route on a stream of its own, and the body
    /// of a 200 response back.
    fn echo(&self, body: Bytes) -> impl Future<Output = Result<Bytes, String>> + Send;
}

/// An error with its causes: "error sending request" alone says nothing
/// about which of the many ways that went wrong.
pub fn error_string(e: &dyn std::error::Error) -> String {
    let mut s = e.to_string();
    let mut source = e.source();
    while let Some(cause) = source {
        s += ": ";
        s += &cause.to_string();
        source = cause.source();
    }
    s
}

// ---------------------------------------------------------------- Connections

pub struct Connections<C> {
    pub conns: Vec<Arc<C>>,
    pub stats: Stats,
    pub concurrency: usize,
}

pub struct DialOptions {
    pub num: usize,
    pub concurrency: usize,
    pub timeout: Duration,
    pub retries: usize,
    pub retry_interval: Duration,
    pub tpn: bool,
}

/// Dials the connections: each one a TCP connection, the HTTP/2 preface and
/// SETTINGS exchanged, and one GET answered on it (Conn::dial). What this
/// measures is the rate the server takes new clients on at.
pub async fn connections<C: Conn>(urls: Vec<String>, o: DialOptions) -> Connections<C> {
    let num = if o.num == 0 { 1000 } else { o.num };
    let concurrency = o.concurrency.clamp(1, num);
    let retries = o.retries.max(1);
    crate::log(&format!("Dial Connections: [{num}]"));
    crate::log(&format!("Dial Concurrency: [{concurrency}]"));

    let conns: Arc<Mutex<Vec<Arc<C>>>> = Arc::new(Mutex::new(Vec::with_capacity(num)));
    let next = Arc::new(AtomicUsize::new(0));
    let urls = Arc::new(urls);

    let progress = {
        let conns = conns.clone();
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(Duration::from_secs(1));
            ticker.tick().await;
            for i in 1.. {
                ticker.tick().await;
                let n = conns.lock().unwrap().len();
                crate::log(&format!("{i:03} seconds passed, {n} Connected ..."));
            }
        })
    };

    crate::log("Connections start ...");
    let (timeout, interval) = (o.timeout, o.retry_interval);
    let dialed = conns.clone();
    let stats = calc::benchmark(concurrency, num, o.tpn, move || {
        let (conns, next, urls) = (dialed.clone(), next.clone(), urls.clone());
        async move {
            let mut err = String::new();
            for i in 0..retries {
                if i > 0 {
                    tokio::time::sleep(interval).await;
                }
                let url = &urls[(next.fetch_add(1, Ordering::Relaxed) + 1) % urls.len()];
                match C::dial(url, timeout).await {
                    Ok(c) => {
                        conns.lock().unwrap().push(Arc::new(c));
                        return Ok(());
                    }
                    Err(e) => err = e,
                }
            }
            Err(err)
        }
    })
    .await;
    progress.abort();
    crate::log(&format!("Connections done: {} Success, {} Failed", stats.success, stats.failed));
    if !stats.errors.is_empty() {
        crate::log(&format!("Connections errors: {:?}", stats.errors));
    }
    let conns = std::mem::take(&mut *conns.lock().unwrap());
    Connections { conns, stats, concurrency }
}

// ------------------------------------------------------------------ BenchEcho

pub struct EchoOptions {
    pub concurrency: usize,
    pub streams: usize,
    pub total: usize,
    pub payload: usize,
    pub limit: usize,
    pub check: bool,
    pub tpn: bool,
}

pub struct Echo {
    pub stats: Stats,
    pub concurrency: usize,
    pub streams: usize,
}

/// Request/response over the connections: a POST of `payload` random bytes to
/// the echo route, on a stream of its own, and the same bytes read back.
/// `concurrency` requests are in flight at once over all connections, and at
/// most `streams` of them on one. `on_warmup` runs as the warmup starts.
pub async fn bench_echo<C: Conn>(conns: &[Arc<C>], o: EchoOptions, on_warmup: impl FnOnce()) -> Echo {
    let streams = o.streams.max(1);
    let concurrency = o.concurrency.clamp(1, (conns.len() * streams).max(1));
    let payload = if o.payload == 0 { 1024 } else { o.payload };

    let mut seed = 0x9e3779b97f4a7c15u64 ^ std::process::id() as u64;
    let bodies: Arc<Vec<Bytes>> = Arc::new((0..1024).map(|_| Bytes::from(random_bytes(&mut seed, payload))).collect());

    // Each connection is in the queue once for every stream it may carry, so
    // that a worker taking one takes one of that connection's streams. They
    // go in round by round, so that the streams in flight are spread over
    // every connection before any carries a second. Every worker holds at
    // most one, and there are no more workers than entries, so the queue is
    // never empty when a worker takes from it.
    let queue: Arc<Mutex<VecDeque<Arc<C>>>> = Arc::new(Mutex::new(
        (0..streams).flat_map(|_| conns.iter().cloned()).collect(),
    ));
    let limiter = Limiter::new(o.limit);
    let pick = Arc::new(AtomicUsize::new(0));
    let check = o.check;
    let once = move || {
        let (queue, bodies, limiter, pick) = (queue.clone(), bodies.clone(), limiter.clone(), pick.clone());
        async move {
            let conn = queue.lock().unwrap().pop_front().expect("more workers than connection streams");
            if let Some(l) = &limiter {
                l.wait().await;
            }
            let i = pick.fetch_add(0x9e37, Ordering::Relaxed) % bodies.len();
            let body = bodies[i].clone();
            let res = conn.echo(body.clone()).await;
            queue.lock().unwrap().push_back(conn);
            let data = res?;
            if check && data != body {
                return Err("response body is not equal to the request's".into());
            }
            if data.len() != body.len() {
                return Err(format!("response body is {} bytes, want {}", data.len(), body.len()));
            }
            Ok(())
        }
    };
    let once = Arc::new(once);

    let warmup_times = (conns.len() * 5).min(2_000_000);
    crate::log(&format!("BenchEcho Warmup for {warmup_times} times ..."));
    on_warmup();
    let f = once.clone();
    calc::warmup(concurrency, warmup_times, move || f()).await;
    crate::log(&format!("BenchEcho Warmup for {warmup_times} times done"));

    crate::log(&format!("BenchEcho for {} times ...", o.total));
    let f = once.clone();
    let stats = calc::benchmark(concurrency, o.total, o.tpn, move || f()).await;
    crate::log(&format!("BenchEcho for {} times done", o.total));
    if !stats.errors.is_empty() {
        crate::log(&format!("BenchEcho errors: {:?}", stats.errors));
    }
    Echo { stats, concurrency, streams }
}

// ------------------------------------------------------------- BenchMultiplex

pub struct RateOptions {
    pub concurrency: usize,
    pub duration: Duration,
    pub send_rate: usize,
    pub batch: usize,
    pub batch_bytes: usize,
    pub payload: usize,
    pub limit: usize,
    pub check: bool,
}

#[derive(Default)]
pub struct Rate {
    pub concurrency: usize,
    pub batch: usize,
    pub send_times: i64,
    pub send_bytes: i64,
    pub recv_times: i64,
    pub recv_bytes: i64,
}

/// How many batches a connection may have unanswered before it is skipped
/// for a tick.
const MAX_BATCHES_IN_FLIGHT: i64 = 4;

/// The size a request is on the wire, near enough to work out how many fit in
/// `-rbs`: the body, its DATA frames' headers, and a HEADERS frame.
pub fn request_len(payload: usize) -> usize {
    payload + 9 * payload.div_ceil(16384).max(1) + 9 + 64
}

/// How many requests go in a batch, and how many batches a second, as the Go
/// client works them out: `-rpl` when it is set, which has to divide
/// `-rr`; otherwise as many as fit in `-rbs` bytes and divide `-rr`.
pub fn batch_size(batch: usize, rate: usize, payload: usize, batch_bytes: usize) -> Result<(usize, usize), String> {
    let rate = rate.max(1);
    if batch > 0 {
        if rate % batch != 0 {
            return Err(format!("batch {batch} does not divide the send rate {rate}, so no whole number of \
                writes a second sends it; pick a divisor of {rate}"));
        }
        return Ok((batch, rate / batch));
    }
    let mut n = (batch_bytes / request_len(payload)).max(1);
    while n > 1 && rate % n != 0 {
        n -= 1;
    }
    Ok((n, rate / n))
}

/// HTTP/2 multiplexing at a rate the client sets: every connection is sent
/// `send_rate` requests a second, a batch of them at a time - each on a
/// stream of its own, all issued together - without waiting for the
/// responses to the ones before. A connection with more than a few batches
/// unanswered is skipped until the server catches up, so a server slower than
/// the rate is measured by what it answered rather than by how deep a queue
/// the client built in front of it. `on_start` runs as the sending starts.
pub async fn bench_rate<C: Conn>(conns: &[Arc<C>], o: RateOptions, on_start: impl FnOnce()) -> Result<Rate, String> {
    let payload = if o.payload == 0 { 1024 } else { o.payload };
    let (batch, tick_rate) = batch_size(o.batch, o.send_rate, payload, o.batch_bytes)?;
    let concurrency = o.concurrency.clamp(1, conns.len().max(1));
    if conns.is_empty() {
        return Err("no connections to run on".into());
    }
    let mut seed = 0x2545f4914f6cdd1du64 ^ std::process::id() as u64;
    let body = Bytes::from(random_bytes(&mut seed, payload));

    struct Counters {
        send_times: AtomicI64,
        send_bytes: AtomicI64,
        recv_times: AtomicI64,
        recv_bytes: AtomicI64,
        answered: AtomicI64,
        // Why the responses that did not count did not, as BenchEcho logs
        // its failures.
        errors: Mutex<BTreeMap<String, usize>>,
    }
    let counters = Arc::new(Counters {
        send_times: AtomicI64::new(0),
        send_bytes: AtomicI64::new(0),
        recv_times: AtomicI64::new(0),
        recv_bytes: AtomicI64::new(0),
        answered: AtomicI64::new(0),
        errors: Mutex::new(BTreeMap::new()),
    });

    let mut teams: Vec<Vec<(Arc<C>, Arc<AtomicI64>)>> = (0..concurrency).map(|_| Vec::new()).collect();
    for (i, c) in conns.iter().enumerate() {
        teams[i % concurrency].push((c.clone(), Arc::new(AtomicI64::new(0))));
    }

    crate::log(&format!("{} for {:.2} seconds, {batch} requests multiplexed per batch ...",
        crate::report::BENCH_MULTIPLEX, o.duration.as_secs_f64()));
    on_start();

    let limiter = Limiter::new(o.limit);
    let tick = Duration::from_secs(1) / tick_rate as u32;
    let deadline = tokio::time::Instant::now() + o.duration;
    let check = o.check;
    let mut writers = Vec::with_capacity(teams.len());
    for team in teams {
        let (counters, body, limiter) = (counters.clone(), body.clone(), limiter.clone());
        writers.push(tokio::spawn(async move {
            let mut ticker = tokio::time::interval_at(tokio::time::Instant::now() + tick, tick);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tokio::select! {
                    _ = tokio::time::sleep_until(deadline) => return,
                    _ = ticker.tick() => {}
                }
                for (conn, in_flight) in &team {
                    if in_flight.load(Ordering::Relaxed) >= batch as i64 * MAX_BATCHES_IN_FLIGHT {
                        continue;
                    }
                    if let Some(l) = &limiter {
                        l.wait_n(batch).await;
                    }
                    counters.send_times.fetch_add(batch as i64, Ordering::Relaxed);
                    counters.send_bytes.fetch_add((batch * body.len()) as i64, Ordering::Relaxed);
                    in_flight.fetch_add(batch as i64, Ordering::Relaxed);
                    // The batch is one task, which opens every stream of it in
                    // its first poll, before the connection's task gets to
                    // write: so the batch goes out together, in as few writes
                    // as the client can, rather than one request per write as
                    // a task each would have it.
                    let requests = (0..batch).map(|_| {
                        let (conn, in_flight, counters, body) = (conn.clone(), in_flight.clone(), counters.clone(), body.clone());
                        async move {
                            let res = conn.echo(body.clone()).await;
                            // Unanswered is unanswered whatever the response
                            // said; only a 200 with the body that was sent
                            // counts as a response in the report.
                            in_flight.fetch_sub(1, Ordering::Relaxed);
                            counters.answered.fetch_add(1, Ordering::Relaxed);
                            let err = match res {
                                Ok(data) if data.len() != body.len() => {
                                    format!("response body is {} bytes, want {}", data.len(), body.len())
                                }
                                Ok(data) if check && data != body => "response body is not equal to the request's".into(),
                                Ok(data) => {
                                    counters.recv_times.fetch_add(1, Ordering::Relaxed);
                                    counters.recv_bytes.fetch_add(data.len() as i64, Ordering::Relaxed);
                                    return;
                                }
                                Err(e) => e,
                            };
                            *counters.errors.lock().unwrap().entry(err).or_default() += 1;
                        }
                    });
                    tokio::spawn(futures_util::future::join_all(requests));
                }
            }
        }));
    }
    for w in writers {
        let _ = w.await;
    }

    // One tick more for the last batch to come back, which is how long the
    // server would have had before the next one, and no longer; whatever
    // arrives after that is not counted.
    let grace = tokio::time::Instant::now() + tick;
    while counters.answered.load(Ordering::Relaxed) < counters.send_times.load(Ordering::Relaxed)
        && tokio::time::Instant::now() < grace
    {
        tokio::time::sleep(Duration::from_millis(1)).await;
    }
    let rate = Rate {
        concurrency,
        batch,
        send_times: counters.send_times.load(Ordering::Relaxed),
        send_bytes: counters.send_bytes.load(Ordering::Relaxed),
        recv_times: counters.recv_times.load(Ordering::Relaxed),
        recv_bytes: counters.recv_bytes.load(Ordering::Relaxed),
    };
    crate::log(&format!("{} for {:.2} seconds done", crate::report::BENCH_MULTIPLEX, o.duration.as_secs_f64()));
    let errors = counters.errors.lock().unwrap();
    if !errors.is_empty() {
        crate::log(&format!("{} errors: {:?}", crate::report::BENCH_MULTIPLEX, *errors));
    }
    Ok(rate)
}

// -------------------------------------------------------------------- helpers

/// At most `per_second` requests a second, spread evenly over it.
struct Limiter {
    interval: Duration,
    next: tokio::sync::Mutex<tokio::time::Instant>,
}

impl Limiter {
    fn new(per_second: usize) -> Option<Arc<Limiter>> {
        (per_second > 0).then(|| {
            Arc::new(Limiter {
                interval: Duration::from_secs(1) / per_second as u32,
                next: tokio::sync::Mutex::new(tokio::time::Instant::now()),
            })
        })
    }

    async fn wait(&self) {
        self.wait_n(1).await
    }

    async fn wait_n(&self, n: usize) {
        let at = {
            let mut next = self.next.lock().await;
            let now = tokio::time::Instant::now();
            let at = (*next).max(now);
            *next = at + self.interval * n as u32;
            at
        };
        tokio::time::sleep_until(at).await;
    }
}

/// xorshift64*: bodies only need to differ, not to be secret.
fn random_bytes(state: &mut u64, n: usize) -> Vec<u8> {
    let mut out = Vec::with_capacity(n + 8);
    while out.len() < n {
        *state ^= *state >> 12;
        *state ^= *state << 25;
        *state ^= *state >> 27;
        out.extend_from_slice(&state.wrapping_mul(0x2545f4914f6cdd1d).to_le_bytes());
    }
    out.truncate(n);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn batches_follow_the_go_client() {
        assert_eq!(batch_size(0, 200, 1024, 16384), Ok((10, 20)));
        assert_eq!(batch_size(25, 200, 1024, 16384), Ok((25, 8)));
        assert!(batch_size(3, 200, 1024, 16384).is_err());
        assert_eq!(batch_size(0, 7, 32 * 1024, 16384), Ok((1, 7)));
    }
}
