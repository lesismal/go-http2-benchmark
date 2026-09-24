//! benchcli-rust: go-http2-benchmark's load client, on reqwest over HTTP/2 in
//! cleartext with prior knowledge. It runs the benchmarks benchcli-go does -
//! Connections, BenchEcho and BenchMultiplex - takes its flags, and writes the
//! same report files, which the Go client's report step turns into the
//! Summary and the tables.

mod bench;
mod calc;
mod config;
mod flags;
mod ps;
mod report;

use std::sync::{Arc, Mutex};
use std::time::Duration;

use report::{BenchEchoReport, BenchRateReport, ConnectionsReport, BENCH_CLIENT, BENCH_MULTIPLEX};

const SHORT_LINE: &str = "--------------------------------------------------------------\n";
const LONG_LINE: &str = "----------------------------------------------------------------------------------------------------\n";

/// logging.Printf: a timestamped line on stderr.
pub fn log(msg: &str) {
    eprintln!("{} {msg}", now());
}

fn print(s: &str) {
    eprint!("{s}");
}

/// logging.NowString's "20060102 15:04.05.000", in UTC: std has no time zone
/// database, and the stamp only orders the lines of one run.
fn now() -> String {
    let d = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as i64;
    let (days, rem) = (secs.div_euclid(86400), secs.rem_euclid(86400));
    // Howard Hinnant's civil_from_days.
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + if month <= 2 { 1 } else { 0 };
    format!("{year:04}{month:02}{day:02} {:02}:{:02}.{:02}.{:03}", rem / 3600, rem % 3600 / 60, rem % 60, d.subsec_millis())
}

fn fatal(msg: &str) -> ! {
    log(msg);
    std::process::exit(1);
}

type Pprof = Arc<Mutex<Option<(Vec<u8>, Vec<u8>)>>>;

/// Fetches a CPU profile of `seconds` and a heap profile, two seconds from
/// now, as the Go client does, into slot.
fn fetch_pprof(control: Arc<ps::Control>, seconds: i64, name: &'static str, slot: Pprof) {
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_secs(2)).await;
        let timeout = Duration::from_secs(seconds.max(0) as u64 + 30);
        let cpu = match control.get(&format!("/debug/pprof/profile?seconds={seconds}"), timeout).await {
            Ok(b) => b,
            Err(e) => return println!("{name}: [pprof cpu] httpGet failed: {e}"),
        };
        let mem = match control.get("/debug/pprof/heap", timeout).await {
            Ok(b) => b,
            Err(e) => return println!("{name}: [pprof mem] httpGet failed: {e}"),
        };
        *slot.lock().unwrap() = Some((cpu, mem));
    });
}

fn save<T: serde::Serialize>(name: &str, kind: &str, r: &T, pprof: Option<(Vec<u8>, Vec<u8>)>, f: &flags::Flags) {
    if let Err(e) = report::to_file(name, r, pprof, &f.str("preffix"), &f.str("suffix")) {
        log(&format!("{name}: writing the {kind} report failed: {e}"));
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let f = match flags::Flags::parse(&args) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(2);
        }
    };
    if let Ok(wd) = std::env::current_dir() {
        log(&format!("pwd: {}", wd.display()));
    }
    if f.bool("r") {
        fatal("benchcli-rust writes the report files; the tables are the Go client's: output/bin/bench.client -r=true");
    }
    let framework = f.str("f");
    let ip = f.str("ip");
    let urls = config::benchmark_urls(&framework, &ip).unwrap_or_else(|| {
        fatal(&format!("-f={framework}: unknown framework, want one of [{}]", config::EXPECTED_FRAMEWORKS))
    });
    if f.bool("rate") {
        if let Err(e) = bench::batch_size(f.int("rpl").max(0) as usize, f.int("rr").max(1) as usize, 1, 1) {
            fatal(&format!("-rpl={}: {e}", f.int("rpl")));
        }
        if f.int("rpl") < 0 {
            fatal(&format!("-rpl={}: want 0, which fits as many requests as -rbs holds, or more", f.int("rpl")));
        }
    }
    if f.str("ps") != "remote" {
        // Accepted, since the scripts pass the Go client's flags to either.
        log(&format!("-ps={}: benchcli-rust reads the server's CPU and MEM from its /ps route (-ps=remote)", f.str("ps")));
    }

    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime")
        .block_on(run(f, framework, ip, urls));
}

async fn run(f: flags::Flags, framework: String, ip: String, urls: Vec<String>) {
    let tpn = f.bool("tpn");
    let payload = f.int("b").max(0) as usize;
    let conns_wanted = f.int("c").max(0) as usize;

    print(LONG_LINE);
    log(&format!("Benchmark [{framework}]: {conns_wanted} connections, {payload} payload, {} times", f.int("en")));
    print(SHORT_LINE);

    // Connections.
    let cs = bench::connections(
        urls,
        bench::DialOptions {
            num: conns_wanted,
            concurrency: f.int("dc").max(1) as usize,
            timeout: f.dur("dt"),
            retries: f.int("dr").max(1) as usize,
            retry_interval: f.dur("dri"),
            tpn,
        },
    )
    .await;
    let s = &cs.stats;
    let cr = ConnectionsReport {
        framework: framework.clone(),
        bench_client: BENCH_CLIENT.into(),
        tps: s.tps(),
        min: s.min,
        avg: s.avg,
        max: s.max,
        tp50: s.tp[0],
        tp75: s.tp[1],
        tp90: s.tp[2],
        tp95: s.tp[3],
        tp99: s.tp[4],
        used: s.used.as_nanos() as i64,
        total: s.total,
        success: s.success,
        failed: s.failed,
        concurrency: cs.concurrency,
        // hyper keeps the SETTINGS the server sent to itself; the scripts
        // pass the servers' -maxstreams to the client as well, and that is
        // what they were started with.
        max_streams: f.int("maxstreams"),
    };
    let name = format!("{framework}-Connections");
    save(&name, "Connections", &cr, None, &f);
    print(SHORT_LINE);
    print(&cr.console(tpn));
    print("\n");
    print(SHORT_LINE);
    if cs.conns.is_empty() {
        fatal("BenchEcho: no connections to run on");
    }

    // The server samples its own CPU and MEM from here on.
    let control = Arc::new(ps::Control::new(config::control_url(&framework, &ip).unwrap()));
    let interval = Duration::from_millis(f.int("pi").max(1) as u64);
    match control.init(interval).await {
        Ok(pid) => log(&format!("{framework}: server pid {pid}, sampled by the server, read from {ip} over /ps")),
        Err(e) => log(&format!("SetupPS({framework}) failed: {e}")),
    }
    // Only a Go server has pprof; any other is not asked for a profile.
    let pprof_enabled = config::has_pprof(&framework);
    if pprof_enabled {
        println!("pprof cpu :\n  curl --output ./cpu_profile {}", control.url("/debug/pprof/profile"));
        println!("  go tool pprof -http=:6060 ./cpu_profile");
        println!("pprof heap:\n  curl --output ./mem_profile {}", control.url("/debug/pprof/heap"));
        println!("  go tool pprof -http=:6061 ./mem_profile");
        print(SHORT_LINE);
    } else {
        log(&format!("{framework}: a {} server has no pprof, not fetching profiles from it", config::lang(&framework)));
    }

    // BenchEcho.
    let echo_pprof: Pprof = Arc::new(Mutex::new(None));
    let total = f.int("en").max(0) as usize;
    let echo = {
        let (control, slot, want, seconds) = (control.clone(), echo_pprof.clone(), f.bool("ep") && pprof_enabled, f.int("epd"));
        bench::bench_echo(
            &cs.conns,
            bench::EchoOptions {
                concurrency: f.int("ec").max(1) as usize,
                streams: f.int("es").max(1) as usize,
                total,
                payload,
                limit: f.int("el").max(0) as usize,
                check: f.bool("check"),
                tpn,
            },
            move || {
                if want {
                    fetch_pprof(control, seconds, "BenchEcho", slot);
                }
            },
        )
        .await
    };
    let s = &echo.stats;
    let (ps, ps_err) = control.ps().await;
    if let Some(e) = ps_err {
        log(&format!("BenchEcho: resource statistics for {framework} incomplete, EER will read 0: {e}"));
    }
    let er = BenchEchoReport {
        framework: framework.clone(),
        bench_client: BENCH_CLIENT.into(),
        tps: s.tps(),
        eer: report::eer(s.tps() as f64, ps.cpu_avg),
        min: s.min,
        avg: s.avg,
        max: s.max,
        tp50: s.tp[0],
        tp75: s.tp[1],
        tp90: s.tp[2],
        tp95: s.tp[3],
        tp99: s.tp[4],
        used: s.used.as_nanos() as i64,
        total,
        success: s.success,
        failed: s.failed,
        conns: cs.conns.len(),
        concurrency: echo.concurrency,
        streams: echo.streams,
        payload: if payload == 0 { 1024 } else { payload },
        pprof: report::pprof_setting(&framework, f.bool("ep") && pprof_enabled, f.int("epd")),
        cpu_min: ps.cpu_min,
        cpu_avg: ps.cpu_avg,
        cpu_max: ps.cpu_max,
        mem_min: ps.mem_min,
        mem_avg: ps.mem_avg,
        mem_max: ps.mem_max,
    };
    let pprof = echo_pprof.lock().unwrap().take();
    save(&format!("{framework}-BenchEcho"), "BenchEcho", &er, pprof, &f);
    print(SHORT_LINE);
    print(&er.console(tpn));
    print("\n");
    print(SHORT_LINE);

    // BenchMultiplex.
    if f.bool("rate") {
        let rate_pprof: Pprof = Arc::new(Mutex::new(None));
        let duration = Duration::from_secs(f.int("rd").max(1) as u64);
        let rate = {
            let (control, slot, want, seconds) = (control.clone(), rate_pprof.clone(), f.bool("rp") && pprof_enabled, f.int("rpd"));
            bench::bench_rate(
                &cs.conns,
                bench::RateOptions {
                    concurrency: f.int("rc").max(1) as usize,
                    duration,
                    send_rate: f.int("rr").max(1) as usize,
                    batch: f.int("rpl").max(0) as usize,
                    batch_bytes: f.int("rbs").max(1) as usize,
                    payload,
                    limit: f.int("rl").max(0) as usize,
                    check: f.bool("check"),
                },
                move || {
                    if want {
                        fetch_pprof(control, seconds, BENCH_MULTIPLEX, slot);
                    }
                },
            )
            .await
            .unwrap_or_else(|e| fatal(&format!("{BENCH_MULTIPLEX}: {e}")))
        };
        let (ps, ps_err) = control.ps().await;
        if let Some(e) = ps_err {
            log(&format!("{BENCH_MULTIPLEX}: resource statistics for {framework} incomplete, EchoEER will read 0: {e}"));
        }
        let tps = rate.recv_times as f64 / duration.as_secs_f64();
        let rr = BenchRateReport {
            framework: framework.clone(),
            bench_client: BENCH_CLIENT.into(),
            duration: duration.as_nanos() as i64,
            tps: tps.floor() as i64,
            echo_eer: report::eer(tps, ps.cpu_avg),
            send_times: rate.send_times,
            send_bytes: rate.send_bytes,
            recv_times: rate.recv_times,
            recv_bytes: rate.recv_bytes,
            conns: cs.conns.len(),
            concurrency: rate.concurrency,
            send_rate: f.int("rr").max(1) as usize,
            batch: rate.batch,
            payload: if payload == 0 { 1024 } else { payload },
            pprof: report::pprof_setting(&framework, f.bool("rp") && pprof_enabled, f.int("rpd")),
            cpu_min: ps.cpu_min,
            cpu_avg: ps.cpu_avg,
            cpu_max: ps.cpu_max,
            mem_min: ps.mem_min,
            mem_avg: ps.mem_avg,
            mem_max: ps.mem_max,
        };
        let pprof = rate_pprof.lock().unwrap().take();
        save(&format!("{framework}-{BENCH_MULTIPLEX}"), BENCH_MULTIPLEX, &rr, pprof, &f);
        print(SHORT_LINE);
        print(&rr.console());
        print("\n");
        print(SHORT_LINE);
    }
    print(LONG_LINE);
    // The connections, and whatever is still in flight on them, go with the
    // process; waiting on each to close would only hold the next framework's
    // turn back.
    std::process::exit(0);
}
