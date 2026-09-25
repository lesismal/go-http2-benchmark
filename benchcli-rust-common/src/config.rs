//! Where each framework's server listens: config.Ports in config/config.go,
//! which config's TestRustClientPorts holds this list to.

pub const EXPECTED_FRAMEWORKS: &str = "beego, chi, echo, fib, gin, goji, gorillamux, h2, hertz, httprouter, nethttp";

/// The first and last benchmark port of a framework. Its control routes,
/// /init, /ps and pprof, are on the port after the last.
pub fn ports(framework: &str) -> Option<(u16, u16)> {
    match framework {
        "beego" => Some((2401, 2450)),
        "chi" => Some((2452, 2501)),
        "echo" => Some((2503, 2552)),
        "fib" => Some((2554, 2603)),
        "gin" => Some((2605, 2654)),
        "goji" => Some((2656, 2705)),
        "gorillamux" => Some((2707, 2756)),
        "h2" => Some((2758, 2807)),
        "hertz" => Some((2809, 2858)),
        "httprouter" => Some((2860, 2909)),
        "nethttp" => Some((2911, 2960)),
        _ => None,
    }
}

/// The language a framework's server is written in: config.Langs, which the
/// report tables show next to the framework, and this client's console too.
pub fn lang(framework: &str) -> &'static str {
    match framework {
        "beego" => "go",
        "chi" => "go",
        "echo" => "go",
        "fib" => "go",
        "gin" => "go",
        "goji" => "go",
        "gorillamux" => "go",
        "h2" => "rust",
        "hertz" => "go",
        "httprouter" => "go",
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
        assert_eq!(urls[0], "http://[::1]:2911/echo");
        assert_eq!(urls[49], "http://[::1]:2960/echo");
        assert_eq!(control_url("fib", "127.0.0.1").unwrap(), "http://127.0.0.1:2604");
        assert_eq!(url_host("[fe80::1]"), "[fe80::1]");
        assert!(ports("gorilla").is_none());
        assert!(has_pprof("nethttp"));
        assert!(!has_pprof("h2"));
    }
}
