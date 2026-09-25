//! benchcli-rust-h2: go-http2-benchmark's load client on hyperium/h2 directly,
//! in cleartext with prior knowledge. Everything but the connection is
//! benchcli-rust-common's.
//!
//! A connection is one TCP connection and one h2 client connection on it,
//! driven by a task of its own; every request is a stream opened on it
//! through a clone of its SendRequest. h2 keeps to the server's
//! SETTINGS_MAX_CONCURRENT_STREAMS by itself: ready() waits until the stream
//! can be opened. This is what reqwest and hyper do underneath, without the
//! pool, the URL handling and the body types they add on top.

use benchcli_rust_common::bench::{error_string, Conn, CONN_WINDOW, STREAM_WINDOW};
use benchcli_rust_common::config::ECHO_PATH;
use bytes::{Bytes, BytesMut};
use h2::client::SendRequest;
use h2::RecvStream;
use http::header::{HeaderValue, CONTENT_LENGTH, CONTENT_TYPE};
use http::{Method, Request, StatusCode, Uri};
use std::time::Duration;
use tokio::net::TcpStream;

static OCTET_STREAM: HeaderValue = HeaderValue::from_static("application/octet-stream");

struct H2Conn {
    send: SendRequest<Bytes>,
    uri: Uri,
}

impl H2Conn {
    /// Sends req, with body as its DATA when there is one, and reads the
    /// response.
    async fn request(&self, req: Request<()>, body: Option<Bytes>) -> Result<(StatusCode, Bytes), String> {
        let mut send = self.send.clone().ready().await.map_err(|e| error_string(&e))?;
        let (res, mut stream) = send.send_request(req, body.is_none()).map_err(|e| error_string(&e))?;
        if let Some(body) = body {
            // Buffered by h2 and sent as the server's window allows.
            stream.send_data(body, true).map_err(|e| error_string(&e))?;
        }
        let res = res.await.map_err(|e| error_string(&e))?;
        let status = res.status();
        let data = read_body(res.into_body()).await?;
        Ok((status, data))
    }
}

/// The whole body, handing each chunk's bytes back to the server's window as
/// it is read. A body that came in one DATA frame is returned as it is.
async fn read_body(mut body: RecvStream) -> Result<Bytes, String> {
    let mut first: Option<Bytes> = None;
    let mut rest: Option<BytesMut> = None;
    while let Some(chunk) = body.data().await {
        let chunk = chunk.map_err(|e| error_string(&e))?;
        let _ = body.flow_control().release_capacity(chunk.len());
        match (&mut rest, first.take()) {
            (Some(buf), _) => buf.extend_from_slice(&chunk),
            (None, Some(prev)) => {
                let mut buf = BytesMut::with_capacity(prev.len() + chunk.len());
                buf.extend_from_slice(&prev);
                buf.extend_from_slice(&chunk);
                rest = Some(buf);
            }
            (None, None) => first = Some(chunk),
        }
    }
    Ok(match rest {
        Some(buf) => buf.freeze(),
        None => first.unwrap_or_default(),
    })
}

impl Conn for H2Conn {
    const BENCH_CLIENT: &'static str = "benchcli-rust-h2";
    const NAME: &'static str = "rust-h2";

    async fn dial(url: &str, timeout: Duration) -> Result<Self, String> {
        let uri: Uri = url.parse().map_err(|e| format!("{url}: {e}"))?;
        let addr = uri.authority().ok_or_else(|| format!("{url}: no host"))?.as_str().to_string();
        let conn = tokio::time::timeout(timeout, async {
            let tcp = TcpStream::connect(&addr).await.map_err(|e| format!("dial {addr}: {e}"))?;
            tcp.set_nodelay(true).map_err(|e| e.to_string())?;
            let (send, connection) = h2::client::Builder::new()
                .initial_window_size(STREAM_WINDOW)
                .initial_connection_window_size(CONN_WINDOW)
                .enable_push(false)
                .handshake::<_, Bytes>(tcp)
                .await
                .map_err(|e| error_string(&e))?;
            // The connection's frames are read and written here, for as long
            // as the process lasts; its end fails the requests on it.
            tokio::spawn(async move {
                let _ = connection.await;
            });
            let conn = H2Conn { send, uri };
            let req = Request::get(conn.uri.clone()).body(()).map_err(|e| e.to_string())?;
            let (status, _) = conn.request(req, None).await?;
            if status != StatusCode::OK {
                return Err(format!("GET {ECHO_PATH}: status {}", status.as_u16()));
            }
            Ok(conn)
        })
        .await
        .map_err(|_| format!("dial {addr}: timed out after {timeout:?}"))??;
        Ok(conn)
    }

    async fn echo(&self, body: Bytes) -> Result<Bytes, String> {
        let mut req = Request::new(());
        *req.method_mut() = Method::POST;
        *req.uri_mut() = self.uri.clone();
        let headers = req.headers_mut();
        headers.insert(CONTENT_TYPE, OCTET_STREAM.clone());
        headers.insert(CONTENT_LENGTH, HeaderValue::from(body.len()));
        let (status, data) = self.request(req, Some(body)).await?;
        if status != StatusCode::OK {
            return Err(format!("status {}", status.as_u16()));
        }
        Ok(data)
    }
}

fn main() {
    benchcli_rust_common::main::<H2Conn>();
}
