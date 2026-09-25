//! benchcli-rust-reqwest: go-http2-benchmark's load client on reqwest, over
//! hyper and h2, in cleartext with prior knowledge. Everything but the
//! connection is benchcli-rust-common's.
//!
//! A reqwest Client keeps one HTTP/2 connection per host and port, and
//! multiplexes every request to it on that one connection. So a connection
//! here is a Client of its own, with one echo URL, holding the one connection
//! its first request opened. hyper, underneath, keeps to the server's
//! SETTINGS_MAX_CONCURRENT_STREAMS by itself.

use benchcli_rust_common::bench::{error_string, Conn, CONN_WINDOW, STREAM_WINDOW};
use benchcli_rust_common::config::ECHO_PATH;
use bytes::Bytes;
use reqwest::header::{HeaderValue, CONTENT_TYPE};
use reqwest::{Client, Url};
use std::time::Duration;

static OCTET_STREAM: HeaderValue = HeaderValue::from_static("application/octet-stream");

struct ReqwestConn {
    client: Client,
    url: Url,
}

impl Conn for ReqwestConn {
    const BENCH_CLIENT: &'static str = "benchcli-rust-reqwest";
    const NAME: &'static str = "rust-reqwest";

    async fn dial(url: &str, timeout: Duration) -> Result<Self, String> {
        let client = Client::builder()
            .http2_prior_knowledge()
            .http2_initial_stream_window_size(STREAM_WINDOW)
            .http2_initial_connection_window_size(CONN_WINDOW)
            .http2_adaptive_window(false)
            .tcp_nodelay(true)
            .no_proxy()
            .connect_timeout(timeout)
            // The connection is the benchmark's for the whole run.
            .pool_idle_timeout(None)
            .build()
            .map_err(|e| e.to_string())?;
        let url = Url::parse(url).map_err(|e| e.to_string())?;
        let res = client.get(url.clone()).timeout(timeout).send().await.map_err(|e| error_string(&e))?;
        let status = res.status();
        res.bytes().await.map_err(|e| error_string(&e))?;
        if status != 200 {
            return Err(format!("GET {ECHO_PATH}: status {}", status.as_u16()));
        }
        Ok(ReqwestConn { client, url })
    }

    async fn echo(&self, body: Bytes) -> Result<Bytes, String> {
        let res = self
            .client
            .post(self.url.clone())
            .header(CONTENT_TYPE, OCTET_STREAM.clone())
            .body(body)
            .send()
            .await
            .map_err(|e| error_string(&e))?;
        let status = res.status();
        let data = res.bytes().await.map_err(|e| error_string(&e))?;
        if status != 200 {
            return Err(format!("status {}", status.as_u16()));
        }
        Ok(data)
    }
}

fn main() {
    benchcli_rust_common::main::<ReqwestConn>();
}
