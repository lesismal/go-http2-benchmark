//! The control routes the Go servers serve on frameworks.StartControlServer:
//! /init, which starts the server sampling its own CPU and memory and answers
//! with its pid, and /ps, which answers with the samples as a
//! github.com/lesismal/perf PSCounter - {"cpu":[...],"mem":[{"rss":...}]} -
//! the only parts of one a client reads. There is no pprof here: a client's
//! -ep and -rp fetches get a 404 and say so.
//!
//! On its own port, on its own threads and over HTTP/1, so that the requests a
//! client reads its resource columns with never wait behind benchmark streams,
//! and the h2 server serves nothing but /echo.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

#[derive(Default)]
struct Samples {
    cpu: Vec<f64>,
    rss: Vec<u64>,
}

pub fn start(port: u16) {
    let ln = match TcpListener::bind(("0.0.0.0", port)) {
        Ok(ln) => ln,
        Err(e) => {
            crate::log(&format!("control server on :{port} exit: {e}"));
            std::process::exit(1);
        }
    };
    let samples = Arc::new(Mutex::new(Samples::default()));
    let started = Arc::new(AtomicBool::new(false));
    std::thread::spawn(move || {
        for stream in ln.incoming().flatten() {
            let (samples, started) = (samples.clone(), started.clone());
            std::thread::spawn(move || {
                let _ = serve(stream, &samples, &started);
            });
        }
    });
}

/// One keep-alive HTTP/1.1 connection, a request at a time.
fn serve(stream: TcpStream, samples: &Arc<Mutex<Samples>>, started: &AtomicBool) -> std::io::Result<()> {
    let mut reader = BufReader::new(stream.try_clone()?);
    let mut writer = stream;
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line)? == 0 {
            return Ok(());
        }
        let path = line.split_whitespace().nth(1).unwrap_or("").to_string();
        let mut content_length = 0usize;
        loop {
            let mut header = String::new();
            if reader.read_line(&mut header)? == 0 {
                return Ok(());
            }
            let header = header.trim_end();
            if header.is_empty() {
                break;
            }
            if let Some((name, value)) = header.split_once(':') {
                if name.eq_ignore_ascii_case("content-length") {
                    content_length = value.trim().parse().unwrap_or(0);
                }
            }
        }
        let mut body = vec![0; content_length];
        reader.read_exact(&mut body)?;

        let (status, reply) = match path.split('?').next().unwrap_or("") {
            "/init" => (200, init(&body, samples, started)),
            "/ps" => (200, ps_json(&samples.lock().unwrap())),
            _ => (404, "404 page not found\n".to_string()),
        };
        let reason = if status == 200 { "OK" } else { "Not Found" };
        write!(writer, "HTTP/1.1 {status} {reason}\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: {}\r\n\r\n{reply}", reply.len())?;
        writer.flush()?;
    }
}

/// Starts sampling, once however many times /init arrives, as the Go
/// servers' handler does: a client that retried it would otherwise start a
/// second sampler appending to the same samples.
fn init(body: &[u8], samples: &Arc<Mutex<Samples>>, started: &AtomicBool) -> String {
    // {"PsInterval": <nanoseconds>}, config.InitArgs as JSON.
    let text = String::from_utf8_lossy(body);
    let interval = text
        .split(':')
        .nth(1)
        .and_then(|v| v.trim_matches(|c: char| !c.is_ascii_digit()).parse::<u64>().ok())
        .filter(|&ns| ns > 0)
        .map(Duration::from_nanos)
        .unwrap_or(Duration::from_secs(1));
    if started.compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst).is_ok() {
        let samples = samples.clone();
        std::thread::spawn(move || sample(interval, &samples));
    } else {
        crate::log("/init called again; the ps counter is already running");
    }
    std::process::id().to_string()
}

/// gopsutil's Percent, which the Go servers sample themselves with: the CPU
/// the process used over each interval, 100 being one core, and its RSS.
fn sample(interval: Duration, samples: &Mutex<Samples>) {
    let (mut last_cpu, mut last_at) = (cpu_time(), Instant::now());
    loop {
        std::thread::sleep(interval);
        let (cpu, at) = (cpu_time(), Instant::now());
        let percent = (cpu - last_cpu).as_secs_f64() / (at - last_at).as_secs_f64() * 100.0;
        (last_cpu, last_at) = (cpu, at);
        let mut s = samples.lock().unwrap();
        s.cpu.push(percent);
        s.rss.push(rss());
    }
}

fn ps_json(s: &Samples) -> String {
    let cpu: Vec<String> = s.cpu.iter().map(|v| format!("{v}")).collect();
    let mem: Vec<String> = s.rss.iter().map(|v| format!("{{\"rss\":{v}}}")).collect();
    format!("{{\"cpu\":[{}],\"mem\":[{}]}}", cpu.join(","), mem.join(","))
}

/// The process' user and system CPU time so far.
fn cpu_time() -> Duration {
    let mut usage: libc::rusage = unsafe { std::mem::zeroed() };
    unsafe { libc::getrusage(libc::RUSAGE_SELF, &mut usage) };
    let tv = |t: libc::timeval| Duration::from_secs(t.tv_sec as u64) + Duration::from_micros(t.tv_usec as u64);
    tv(usage.ru_utime) + tv(usage.ru_stime)
}

/// The process' resident set size now, in bytes.
#[cfg(target_os = "linux")]
fn rss() -> u64 {
    let statm = std::fs::read_to_string("/proc/self/statm").unwrap_or_default();
    let pages: u64 = statm.split_whitespace().nth(1).and_then(|v| v.parse().ok()).unwrap_or(0);
    pages * unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as u64
}

#[cfg(target_os = "macos")]
fn rss() -> u64 {
    let mut info: libc::proc_taskinfo = unsafe { std::mem::zeroed() };
    let size = std::mem::size_of::<libc::proc_taskinfo>() as libc::c_int;
    let n = unsafe {
        libc::proc_pidinfo(std::process::id() as libc::c_int, libc::PROC_PIDTASKINFO, 0, &mut info as *mut _ as *mut libc::c_void, size)
    };
    if n == size {
        info.pti_resident_size
    } else {
        0
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
fn rss() -> u64 {
    0
}

/// logging.NowString's "20060102 15:04.05.000", in UTC.
pub fn now() -> String {
    let d = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap_or_default();
    let secs = d.as_secs() as i64;
    let (days, rem) = (secs.div_euclid(86400), secs.rem_euclid(86400));
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
