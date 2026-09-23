//! github.com/lesismal/perf's Calculator, which the Go client measures with,
//! so that the two clients' Connections and BenchEcho numbers mean the same
//! thing: `concurrency` tasks take turns at `times` calls, each timed; TPS is
//! the successes per second of the whole run, and Min, Avg, Max and the TP
//! percentiles are over the successful calls only. As in perf, no latency is
//! recorded, and Min, Avg and Max read 0, when the percentiles are off.

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

pub const PERCENTS: [usize; 5] = [50, 75, 90, 95, 99];

#[derive(Default, Debug)]
pub struct Stats {
    pub total: usize,
    pub used: Duration,
    pub min: i64,
    pub avg: i64,
    pub max: i64,
    pub success: i64,
    pub failed: i64,
    /// TP50, TP75, TP90, TP95 and TP99, in PERCENTS order.
    pub tp: [i64; 5],
    pub errors: BTreeMap<String, usize>,
}

impl Stats {
    /// perf's TPS: successes over the seconds the whole run took, truncated.
    pub fn tps(&self) -> i64 {
        let secs = self.used.as_secs_f64();
        if secs <= 0.0 {
            return 0;
        }
        (self.success as f64 / secs) as i64
    }
}

/// Runs f `times` times on `concurrency` tasks. Latencies are recorded only
/// when `record` is set, as perf records them only for the percentiles.
pub async fn benchmark<F, Fut>(concurrency: usize, times: usize, record: bool, f: F) -> Stats
where
    F: Fn() -> Fut + Send + Sync + 'static,
    Fut: Future<Output = Result<(), String>> + Send + 'static,
{
    let f = Arc::new(f);
    let counter = Arc::new(AtomicUsize::new(0));
    let begin = Instant::now();
    let mut tasks = Vec::with_capacity(concurrency);
    for _ in 0..concurrency.max(1) {
        let f = f.clone();
        let counter = counter.clone();
        tasks.push(tokio::spawn(async move {
            let mut costs: Vec<i64> = Vec::new();
            let mut errors: BTreeMap<String, usize> = BTreeMap::new();
            let (mut success, mut failed) = (0i64, 0i64);
            loop {
                let cnt = counter.fetch_add(1, Ordering::Relaxed) + 1;
                if cnt > times {
                    break;
                }
                let t = Instant::now();
                match f().await {
                    Ok(()) => {
                        success += 1;
                        if record {
                            costs.push(t.elapsed().as_nanos() as i64);
                        }
                    }
                    Err(e) => {
                        failed += 1;
                        *errors.entry(e).or_default() += 1;
                    }
                }
            }
            (costs, errors, success, failed)
        }));
    }
    let mut stats = Stats { total: times, ..Default::default() };
    let mut costs: Vec<i64> = Vec::with_capacity(if record { times } else { 0 });
    for task in tasks {
        let (c, e, s, fl) = task.await.expect("benchmark task panicked");
        costs.extend(c);
        for (k, v) in e {
            *stats.errors.entry(k).or_default() += v;
        }
        stats.success += s;
        stats.failed += fl;
    }
    stats.used = begin.elapsed();
    calculate(&mut stats, costs);
    stats
}

/// Warmup is Benchmark with nothing recorded.
pub async fn warmup<F, Fut>(concurrency: usize, times: usize, f: F)
where
    F: Fn() -> Fut + Send + Sync + 'static,
    Fut: Future<Output = Result<(), String>> + Send + 'static,
{
    benchmark(concurrency, times, false, f).await;
}

fn calculate(stats: &mut Stats, mut costs: Vec<i64>) {
    costs.retain(|&c| c > 0);
    if costs.is_empty() {
        return;
    }
    costs.sort_unstable();
    stats.min = costs[0];
    stats.max = costs[costs.len() - 1];
    stats.avg = (costs.iter().map(|&c| c as i128).sum::<i128>() / costs.len() as i128) as i64;
    for (i, p) in PERCENTS.iter().enumerate() {
        // perf's TPNFrom: the entry p percent of the way into the sorted
        // successes, the last one at most.
        let idx = ((*p as f64 / 100.0) * costs.len() as f64) as usize;
        stats.tp[i] = costs[idx.min(costs.len() - 1)];
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn percentiles_follow_perf() {
        let mut stats = Stats::default();
        calculate(&mut stats, (1..=100).rev().collect());
        assert_eq!((stats.min, stats.max, stats.avg), (1, 100, 50));
        assert_eq!(stats.tp, [51, 76, 91, 96, 100]);
    }

    #[tokio::test]
    async fn counts_every_call_once() {
        let stats = benchmark(8, 1000, true, || async { Ok(()) }).await;
        assert_eq!((stats.success, stats.failed), (1000, 0));
        let stats = benchmark(3, 10, false, || async { Err("no".to_string()) }).await;
        assert_eq!((stats.success, stats.failed, stats.errors["no"]), (0, 10, 10));
        assert_eq!(stats.avg, 0);
    }
}
