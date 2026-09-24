//! Where each framework's server listens: config.Ports in config/config.go,
//! which config's TestRustClientPorts holds this list to.

pub const EXPECTED_FRAMEWORKS: &str = "fib, gin, h2, nethttp";

/// The first and last benchmark port of a framework. Its control routes,
/// /init, /ps and pprof, are on the port after the last.
pub fn ports(framework: &str) -> Option<(u16, u16)> {
    match framework {
        "fib" => Some((21001, 21050)),
        "gin" => Some((22001, 22050)),
        "h2" => Some((23001, 23050)),
        "nethttp" => Some((24001, 24050)),
        _ => None,
    }
}

/// The language a framework's server is written in: config.Langs, which the
/// report tables show next to the framework, and this client's console too.
pub fn lang(framework: &str) -> &'static str {
    match framework {
        "fib" => "go",
        "gin" => "go",
        "h2" => "rust",
        "nethttp" => "go",
        _ => "",
    }
}

/// Whether a framework's control server serves /debug/pprof/: config.HasPprof.
/// Only the Go ones do, so no other is asked for a profile.
pub fn has_pprof(framework: &str) -> bool {
    lang(framework) == "go"
}

pub const ECHO_PATH: &str = "/echo";

/// host as it goes in a URL: an IPv6 address in brackets.
pub fn url_host(ip: &str) -> String {
    if ip.contains(':') && !ip.starts_with('[') {
        format!("[{ip}]")
    } else {
        ip.to_string()
    }
}

/// The echo URL on every benchmark port of the framework.
pub fn benchmark_urls(framework: &str, ip: &str) -> Option<Vec<String>> {
    let (first, last) = ports(framework)?;
    let host = url_host(ip);
    Some((first..=last).map(|p| format!("http://{host}:{p}{ECHO_PATH}")).collect())
}

/// The base URL of the framework's control routes.
pub fn control_url(framework: &str, ip: &str) -> Option<String> {
    let (_, last) = ports(framework)?;
    Some(format!("http://{}:{}", url_host(ip), last + 1))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls() {
        let urls = benchmark_urls("nethttp", "::1").unwrap();
        assert_eq!(urls.len(), 50);
        assert_eq!(urls[0], "http://[::1]:24001/echo");
        assert_eq!(urls[49], "http://[::1]:24050/echo");
        assert_eq!(control_url("fib", "127.0.0.1").unwrap(), "http://127.0.0.1:21051");
        assert_eq!(url_host("[fe80::1]"), "[fe80::1]");
        assert!(ports("gorilla").is_none());
        assert!(has_pprof("nethttp"));
        assert!(!has_pprof("h2"));
    }
}
