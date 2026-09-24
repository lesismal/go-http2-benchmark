//! The client's command line, in Go's flag syntax, since script/benchmark.sh
//! hands every client the same arguments: `-name=value`, `-name value` for
//! anything but a bool, a bare `-name` for a bool, and `--name` for any of
//! them. A flag the Go client defines and this one has no use for is still
//! accepted, so that the scripts can pass either client the same line.

use std::collections::HashMap;
use std::time::Duration;

#[derive(Clone, Copy, PartialEq)]
enum Kind {
    Bool,
    Int,
    Str,
    Dur,
}

struct Def {
    name: &'static str,
    kind: Kind,
    default: &'static str,
    usage: &'static str,
}

// The Go client's flags, in its order, with its defaults.
const DEFS: &[Def] = &[
    Def { name: "nodelay", kind: Kind::Bool, default: "true", usage: "server: tcp nodelay; the client always sets it" },
    Def { name: "reuseport", kind: Kind::Bool, default: "true", usage: "server: reuse port (unused here)" },
    Def { name: "maxstreams", kind: Kind::Int, default: "250", usage: "server: HTTP/2 max concurrent streams per connection, recorded as Max Streams" },
    Def { name: "m", kind: Kind::Int, default: "4294967296", usage: "memory limit (unused here)" },
    Def { name: "f", kind: Kind::Str, default: "nethttp", usage: "framework, e.g. \"nethttp\"" },
    Def { name: "ip", kind: Kind::Str, default: "127.0.0.1", usage: "ip, e.g. \"127.0.0.1\"" },
    Def { name: "c", kind: Kind::Int, default: "10000", usage: "client: num of connections" },
    Def { name: "dc", kind: Kind::Int, default: "2000", usage: "client: dial concurrency: how many tasks dial at once" },
    Def { name: "dt", kind: Kind::Dur, default: "5s", usage: "client: dial timeout, which also bounds the first request on a connection" },
    Def { name: "dr", kind: Kind::Int, default: "5", usage: "client: dial retry times" },
    Def { name: "dri", kind: Kind::Dur, default: "100ms", usage: "client: dial retry interval" },
    Def { name: "b", kind: Kind::Int, default: "1024", usage: "benchmark: request body size of benchecho and benchrate, which the server echoes back" },
    Def { name: "check", kind: Kind::Bool, default: "false", usage: "benchmark: whether to check the validity of the response data" },
    Def { name: "pi", kind: Kind::Int, default: "1000", usage: "benchmark: ps interval of benchecho and benchrate, in ms" },
    Def { name: "ps", kind: Kind::Str, default: "auto", usage: "benchmark: where the server's CPU and MEM samples come from; this client always asks the server over /ps" },
    Def { name: "tpn", kind: Kind::Bool, default: "true", usage: "benchmark: whether enable TPN caculation" },
    Def { name: "ec", kind: Kind::Int, default: "10000", usage: "benchecho: concurrency: how many tasks do the echo test, each with one request in flight" },
    Def { name: "es", kind: Kind::Int, default: "1", usage: "benchecho: streams: how many requests one connection carries in flight at once; -ec is capped at -c times this" },
    Def { name: "en", kind: Kind::Int, default: "2000000", usage: "benchecho: benchmark times" },
    Def { name: "el", kind: Kind::Int, default: "0", usage: "benchecho: TPS limitation per second" },
    Def { name: "ep", kind: Kind::Bool, default: "false", usage: "benchecho: generate pprof report" },
    Def { name: "epd", kind: Kind::Int, default: "5", usage: "benchecho: pprof duration" },
    Def { name: "rate", kind: Kind::Bool, default: "false", usage: "benchrate: whether run benchrate" },
    Def { name: "rc", kind: Kind::Int, default: "10000", usage: "benchrate: concurrency: how many tasks write the multiplexed requests" },
    Def { name: "rd", kind: Kind::Int, default: "10", usage: "benchrate: how long to spend to do the test, in seconds" },
    Def { name: "rr", kind: Kind::Int, default: "200", usage: "benchrate: how many requests can be sent to 1 conn every second" },
    Def { name: "rbs", kind: Kind::Int, default: "16384", usage: "benchrate: how many bytes of requests go to 1 conn every time, when -rpl is 0" },
    Def { name: "rpl", kind: Kind::Int, default: "0", usage: "benchrate: batch: how many requests, one stream each, go to 1 conn at once, which must divide -rr; 0 takes as many as fit in -rbs bytes" },
    Def { name: "rl", kind: Kind::Int, default: "0", usage: "benchrate: request sending limitation per second" },
    Def { name: "rp", kind: Kind::Bool, default: "false", usage: "benchrate: generate pprof report" },
    Def { name: "rpd", kind: Kind::Int, default: "5", usage: "benchrate: pprof duration" },
    Def { name: "r", kind: Kind::Bool, default: "false", usage: "make report: that is the Go client's (output/bin/bench.client -r=true)" },
    Def { name: "preffix", kind: Kind::Str, default: "", usage: "report file preffix, e.g. \"1m_connections_\"" },
    Def { name: "suffix", kind: Kind::Str, default: "", usage: "report file suffix, e.g. \"_20060102150405\"" },
    Def { name: "sort", kind: Kind::Str, default: "result", usage: "report row order (the Go client's; unused here)" },
];

pub struct Flags {
    values: HashMap<&'static str, String>,
}

impl Flags {
    /// Parses args, which do not include the program name.
    pub fn parse(args: &[String]) -> Result<Flags, String> {
        let mut values: HashMap<&'static str, String> =
            DEFS.iter().map(|d| (d.name, d.default.to_string())).collect();
        let mut i = 0;
        while i < args.len() {
            let arg = &args[i];
            i += 1;
            let body = arg
                .strip_prefix("--")
                .or_else(|| arg.strip_prefix('-'))
                .filter(|b| !b.is_empty() && !b.starts_with('-'))
                .ok_or_else(|| format!("bad flag syntax: {arg}"))?;
            if body == "h" || body == "help" {
                return Err(usage());
            }
            let (name, inline) = match body.split_once('=') {
                Some((n, v)) => (n, Some(v.to_string())),
                None => (body, None),
            };
            let def = DEFS
                .iter()
                .find(|d| d.name == name)
                .ok_or_else(|| format!("flag provided but not defined: -{name}\n{}", usage()))?;
            let value = match (inline, def.kind) {
                (Some(v), _) => v,
                (None, Kind::Bool) => "true".to_string(),
                (None, _) => {
                    let v = args.get(i).ok_or_else(|| format!("flag needs an argument: -{name}"))?;
                    i += 1;
                    v.clone()
                }
            };
            check(def, &value)?;
            values.insert(def.name, value);
        }
        Ok(Flags { values })
    }

    pub fn str(&self, name: &str) -> String {
        self.values[name].clone()
    }

    pub fn int(&self, name: &str) -> i64 {
        self.values[name].parse().unwrap()
    }

    pub fn bool(&self, name: &str) -> bool {
        parse_bool(&self.values[name]).unwrap()
    }

    pub fn dur(&self, name: &str) -> Duration {
        parse_duration(&self.values[name]).unwrap()
    }
}

fn check(def: &Def, value: &str) -> Result<(), String> {
    let ok = match def.kind {
        Kind::Bool => parse_bool(value).is_some(),
        Kind::Int => value.parse::<i64>().is_ok(),
        Kind::Dur => parse_duration(value).is_some(),
        Kind::Str => true,
    };
    if ok {
        Ok(())
    } else {
        Err(format!("invalid value {value:?} for flag -{}", def.name))
    }
}

fn usage() -> String {
    let mut s = String::from("Usage of benchcli-rust:\n");
    for d in DEFS {
        s += &format!("  -{}\n    \t{} (default {:?})\n", d.name, d.usage, d.default);
    }
    s
}

fn parse_bool(s: &str) -> Option<bool> {
    match s {
        "1" | "t" | "T" | "true" | "TRUE" | "True" => Some(true),
        "0" | "f" | "F" | "false" | "FALSE" | "False" => Some(false),
        _ => None,
    }
}

/// Go's time.ParseDuration: a sequence of decimal numbers, each with a unit,
/// such as "300ms" or "1m30s"; "0" alone needs none.
pub fn parse_duration(s: &str) -> Option<Duration> {
    if s == "0" {
        return Some(Duration::ZERO);
    }
    let mut rest = s;
    let mut total = 0f64;
    if rest.is_empty() {
        return None;
    }
    while !rest.is_empty() {
        let num_len = rest.find(|c: char| !(c.is_ascii_digit() || c == '.'))?;
        let num: f64 = rest[..num_len].parse().ok()?;
        rest = &rest[num_len..];
        let unit_len = rest.find(|c: char| c.is_ascii_digit() || c == '.').unwrap_or(rest.len());
        let scale = match &rest[..unit_len] {
            "ns" => 1.0,
            "us" | "µs" => 1e3,
            "ms" => 1e6,
            "s" => 1e9,
            "m" => 60e9,
            "h" => 3600e9,
            _ => return None,
        };
        total += num * scale;
        rest = &rest[unit_len..];
    }
    Some(Duration::from_nanos(total as u64))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn go_flag_syntax() {
        let f = Flags::parse(&args(&["-c=100", "--ip", "::1", "-check", "-rate=false", "-dt", "1m30s", "-maxstreams=10"])).unwrap();
        assert_eq!(f.int("c"), 100);
        assert_eq!(f.str("ip"), "::1");
        assert!(f.bool("check"));
        assert!(!f.bool("rate"));
        assert_eq!(f.dur("dt"), Duration::from_secs(90));
        assert_eq!(f.dur("dri"), Duration::from_millis(100));
        assert!(Flags::parse(&args(&["-nosuch=1"])).is_err());
        assert!(Flags::parse(&args(&["-c=ten"])).is_err());
    }

    #[test]
    fn durations() {
        assert_eq!(parse_duration("1.5s"), Some(Duration::from_millis(1500)));
        assert_eq!(parse_duration("250us"), Some(Duration::from_micros(250)));
        assert_eq!(parse_duration("0"), Some(Duration::ZERO));
        assert_eq!(parse_duration("5"), None);
        assert_eq!(parse_duration("5x"), None);
    }
}
