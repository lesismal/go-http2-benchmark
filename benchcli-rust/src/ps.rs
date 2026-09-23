//! The server's CPU and memory, as the server samples them itself: /init
//! starts its sampler and answers with its pid, and /ps answers with the
//! samples so far, as a github.com/lesismal/perf PSCounter. This client always
//! reads them that way, the Go client's -ps=remote; the Go client can also
//! sample a server on its own machine directly.
//!
//! Control requests go to a port of their own, over HTTP/1 on a client of
//! their own, but still arrive at a process that may be working through the
//! backlog of a just-finished benchmark. So they are retried, patiently, as the
//! Go client retries them (config.controlRequest).

use serde::Deserialize;
use std::time::Duration;

const ATTEMPTS: u32 = 4;
const TIMEOUT: Duration = Duration::from_secs(30);
const BACKOFF: Duration = Duration::from_secs(2);

pub struct Control {
    client: reqwest::Client,
    base: String,
}

#[derive(Deserialize, Default)]
struct Counter {
    #[serde(default)]
    cpu: Option<Vec<f64>>,
    #[serde(default)]
    mem: Option<Vec<Mem>>,
}

#[derive(Deserialize)]
struct Mem {
    #[serde(default)]
    rss: u64,
}

/// What a report's resource columns read.
#[derive(Default, Clone, Copy)]
pub struct PsStats {
    pub cpu_min: f64,
    pub cpu_avg: f64,
    pub cpu_max: f64,
    pub mem_min: u64,
    pub mem_avg: u64,
    pub mem_max: u64,
}

impl Control {
    pub fn new(base: String) -> Control {
        let client = reqwest::Client::builder()
            .http1_only()
            .no_proxy()
            .timeout(TIMEOUT)
            .build()
            .expect("control client");
        Control { client, base }
    }

    pub fn url(&self, path: &str) -> String {
        format!("{}{}", self.base, path)
    }

    /// Starts the server sampling itself every `interval`, and returns its pid.
    pub async fn init(&self, interval: Duration) -> Result<i64, String> {
        let body = format!("{{\"PsInterval\":{}}}", interval.as_nanos());
        let data = self.request("/init", Some(body)).await?;
        let text = String::from_utf8_lossy(&data);
        text.trim().parse().map_err(|e| format!("{}: pid {text:?}: {e}", self.url("/init")))
    }

    /// The samples so far. Samples that arrived come back with an error too,
    /// when there are too few of them to be what the phase should have had.
    pub async fn ps(&self) -> (PsStats, Option<String>) {
        let data = match self.request("/ps", None).await {
            Ok(d) => d,
            Err(e) => return (PsStats::default(), Some(e)),
        };
        let counter: Counter = match serde_json::from_slice(&data) {
            Ok(c) => c,
            Err(e) => return (PsStats::default(), Some(format!("{}: {e}", self.url("/ps")))),
        };
        let stats = stats(counter.cpu.unwrap_or_default(), counter.mem.unwrap_or_default().iter().map(|m| m.rss).collect());
        if stats.cpu_avg <= 0.0 {
            return (stats, Some(format!("{}: answered with no CPU samples, so either /init did not reach it \
                or the phase was shorter than the -pi sampling interval", self.url("/ps"))));
        }
        (stats, None)
    }

    /// Fetches one pprof profile, which takes as long as it samples for.
    pub async fn get(&self, path: &str, timeout: Duration) -> Result<Vec<u8>, String> {
        let url = self.url(path);
        let res = self.client.get(&url).timeout(timeout).send().await.map_err(|e| format!("{url}: {e}"))?;
        if !res.status().is_success() {
            return Err(format!("{url}: {}", res.status()));
        }
        res.bytes().await.map(|b| b.to_vec()).map_err(|e| format!("{url}: {e}"))
    }

    async fn request(&self, path: &str, body: Option<String>) -> Result<Vec<u8>, String> {
        let url = self.url(path);
        let mut last = String::new();
        for attempt in 1..=ATTEMPTS {
            if attempt > 1 {
                tokio::time::sleep(BACKOFF * (attempt - 1)).await;
            }
            let req = match &body {
                Some(b) => self.client.post(&url).body(b.clone()),
                None => self.client.get(&url),
            };
            match req.send().await {
                Ok(res) => {
                    let status = res.status();
                    match res.bytes().await {
                        // A reply the server produced is returned as it is:
                        // waiting will not put a missing route there.
                        Ok(data) if status.is_success() => return Ok(data.to_vec()),
                        Ok(data) => return Err(format!("{url}: {status}: {}", String::from_utf8_lossy(&data).trim())),
                        Err(e) => last = format!("{url}: {e}"),
                    }
                }
                Err(e) => last = format!("{url}: {e}"),
            }
            if attempt < ATTEMPTS {
                crate::log(&format!("control request failed, retrying ({attempt}/{ATTEMPTS}): {last}"));
            }
        }
        Err(last)
    }
}

/// perf.PSCounter's statistics, in the order the Go client reads them:
/// the first CPU sample, taken over a partial interval, is left out of Min
/// and Avg; MEMRSSMin sorts the memory samples and takes the second
/// smallest, which leaves MEMRSSAvg over all but the smallest.
fn stats(cpu: Vec<f64>, mut rss: Vec<u64>) -> PsStats {
    let mut s = PsStats::default();
    match cpu.len() {
        0 => {}
        1 => (s.cpu_min, s.cpu_avg, s.cpu_max) = (cpu[0], cpu[0], cpu[0].max(0.0)),
        n => {
            s.cpu_min = cpu[1..].iter().cloned().fold(f64::MAX, f64::min);
            s.cpu_avg = cpu[1..].iter().sum::<f64>() / (n - 1) as f64;
            s.cpu_max = cpu.iter().cloned().fold(0.0, f64::max);
        }
    }
    rss.sort_unstable();
    match rss.len() {
        0 => {}
        1 => (s.mem_min, s.mem_avg) = (rss[0], rss[0]),
        n => {
            s.mem_min = rss[1];
            s.mem_avg = rss[1..].iter().sum::<u64>() / (n - 1) as u64;
        }
    }
    s.mem_max = rss.last().cloned().unwrap_or(0);
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stats_follow_perf() {
        let s = stats(vec![900.0, 100.0, 300.0], vec![30, 10, 20]);
        assert_eq!((s.cpu_min, s.cpu_avg, s.cpu_max), (100.0, 200.0, 900.0));
        assert_eq!((s.mem_min, s.mem_avg, s.mem_max), (20, 25, 30));
        let s = stats(vec![], vec![]);
        assert_eq!(s.cpu_avg, 0.0);
        let counter: Counter = serde_json::from_str(r#"{"cpu":[1.5],"mem":[{"rss":7,"vms":9}],"io":null}"#).unwrap();
        assert_eq!(counter.mem.unwrap()[0].rss, 7);
    }
}
