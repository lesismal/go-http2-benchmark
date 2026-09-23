//! The h2 framework: github.com/hyperium/h2's server, on tokio, answering
//! `POST /echo` with the request body, the way the Go servers do.
//!
//! It is what the Go servers are in everything but the language: the flags
//! script/benchmark.sh forwards to them, HTTP/2 in cleartext with prior
//! knowledge and nothing else on fifty benchmark ports, and the control routes
//! /init and /ps on the port after the last, on a small HTTP/1 server of their
//! own (see control.rs). It serves h2 directly, not through hyper, so that the
//! row is the HTTP/2 library and not a framework on top of it.

mod control;

use bytes::{Bytes, BytesMut};
use h2::server::{self, SendResponse};
use h2::RecvStream;
use http::{Request, Response, StatusCode};
use std::net::SocketAddr;
use std::time::Duration;
use tokio::net::{TcpListener, TcpSocket, TcpStream};

const FRAMEWORK: &str = "h2";
const FIRST_PORT: u16 = 23001;
const LAST_PORT: u16 = 23050;
const ECHO_PATH: &str = "/echo";

/// The flow-control windows the server gives a client for its request bodies,
/// per stream and per connection: net/http's defaults
/// (MaxUploadBufferPerStream and MaxUploadBufferPerConnection), where h2's
/// own are RFC 9113's 64KB.
const STREAM_WINDOW: u32 = 1 << 20;
const CONN_WINDOW: u32 = 1 << 20;

struct Options {
    nodelay: bool,
    reuseport: bool,
    payload: usize,
    max_streams: u32,
}

pub fn log(msg: &str) {
    eprintln!("{} {msg}", control::now());
}

fn main() {
    let o = match parse_flags(std::env::args().skip(1)) {
        Ok(o) => o,
        Err(e) => {
            eprintln!("{e}");
            std::process::exit(2);
        }
    };
    log(&format!(
        "{FRAMEWORK} server: nodelay={}, reuseport={}, payload={}, max streams={}",
        o.nodelay, o.reuseport, o.payload, o.max_streams
    ));
    control::start(LAST_PORT + 1);

    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime")
        .block_on(serve(o));
}

async fn serve(o: Options) {
    let o = std::sync::Arc::new(o);
    for port in FIRST_PORT..=LAST_PORT {
        let ln = match listen(port, o.reuseport) {
            Ok(ln) => ln,
            Err(e) => {
                log(&format!("listen :{port} failed: {e}"));
                std::process::exit(1);
            }
        };
        let o = o.clone();
        tokio::spawn(async move {
            loop {
                match ln.accept().await {
                    Ok((socket, _)) => {
                        let o = o.clone();
                        tokio::spawn(async move { serve_conn(socket, &o).await });
                    }
                    Err(e) => {
                        // Out of descriptors, most likely: wait for some to
                        // come back rather than spin.
                        log(&format!("accept :{port}: {e}"));
                        tokio::time::sleep(Duration::from_millis(10)).await;
                    }
                }
            }
        });
    }
    log(&format!("{FRAMEWORK} server: listening on {} ports, :{FIRST_PORT} to :{LAST_PORT}", LAST_PORT - FIRST_PORT + 1));

    // SIGINT is what script/killone.sh stops a server with; SIGTERM too.
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).expect("SIGTERM handler");
    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        _ = term.recv() => {}
    }
    log(&format!("{FRAMEWORK} server: exit"));
    std::process::exit(0);
}

fn listen(port: u16, reuseport: bool) -> std::io::Result<TcpListener> {
    let addr: SocketAddr = ([0, 0, 0, 0], port).into();
    let socket = TcpSocket::new_v4()?;
    socket.set_reuseaddr(true)?;
    if reuseport {
        socket.set_reuseport(true)?;
    }
    socket.bind(addr)?;
    socket.listen(4096)
}

async fn serve_conn(socket: TcpStream, o: &Options) {
    if o.nodelay {
        let _ = socket.set_nodelay(true);
    }
    let mut conn = match server::Builder::new()
        .max_concurrent_streams(o.max_streams)
        .initial_window_size(STREAM_WINDOW)
        .initial_connection_window_size(CONN_WINDOW)
        .handshake::<_, Bytes>(socket)
        .await
    {
        Ok(conn) => conn,
        Err(_) => return,
    };
    let payload = o.payload;
    // A task per stream, as net/http gives each stream a goroutine: the
    // streams open on one connection are served at once, not one after
    // another. accept() also drives the connection's I/O, so the loop runs
    // until the client goes away.
    while let Some(Ok((req, respond))) = conn.accept().await {
        tokio::spawn(async move {
            let _ = handle(req, respond, payload).await;
        });
    }
}

async fn handle(req: Request<RecvStream>, mut respond: SendResponse<Bytes>, payload: usize) -> Result<(), h2::Error> {
    if req.uri().path() != ECHO_PATH {
        let res = Response::builder()
            .status(StatusCode::NOT_FOUND)
            .header("content-type", "text/plain; charset=utf-8")
            .body(())
            .unwrap();
        let mut send = respond.send_response(res, false)?;
        return send.send_data(Bytes::from_static(b"404 page not found\n"), true);
    }

    let mut body = req.into_body();
    let mut buf = BytesMut::with_capacity(payload);
    while let Some(chunk) = body.data().await {
        let chunk = chunk?;
        // Handing the window back as the body is read is what lets the
        // client send the rest of it, and the next request.
        let _ = body.flow_control().release_capacity(chunk.len());
        buf.extend_from_slice(&chunk);
    }

    let res = Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "application/octet-stream")
        .header("content-length", buf.len())
        .body(())
        .unwrap();
    if buf.is_empty() {
        respond.send_response(res, true)?;
        return Ok(());
    }
    let mut send = respond.send_response(res, false)?;
    // h2 queues what the client's window does not yet let through, and sends
    // it as WINDOW_UPDATEs arrive.
    send.send_data(buf.freeze(), true)
}

/// The Go servers' flags (frameworks.Init), in Go's syntax: -name=value, or a
/// bare -name for a bool. -m, the Go servers' memory limit, is accepted and
/// has nothing to limit here.
fn parse_flags(args: impl Iterator<Item = String>) -> Result<Options, String> {
    let mut o = Options { nodelay: true, reuseport: true, payload: 1024, max_streams: 250 };
    for arg in args {
        let body = arg.trim_start_matches('-');
        let (name, value) = match body.split_once('=') {
            Some((n, v)) => (n, Some(v)),
            None => (body, None),
        };
        let bool_value = |v: Option<&str>| match v {
            None | Some("1" | "t" | "T" | "true" | "TRUE" | "True") => Ok(true),
            Some("0" | "f" | "F" | "false" | "FALSE" | "False") => Ok(false),
            Some(v) => Err(format!("invalid boolean value {v:?} for -{name}")),
        };
        let int_value = |v: Option<&str>| {
            v.ok_or(format!("flag needs an argument: -{name}"))?
                .parse::<i64>()
                .map_err(|e| format!("invalid value for -{name}: {e}"))
        };
        match name {
            "nodelay" => o.nodelay = bool_value(value)?,
            "reuseport" => o.reuseport = bool_value(value)?,
            "b" => o.payload = int_value(value)?.max(0) as usize,
            "m" => {
                int_value(value)?;
            }
            "maxstreams" => {
                let n = int_value(value)?;
                if n <= 0 {
                    return Err(format!("-maxstreams={n}: want at least 1"));
                }
                o.max_streams = n.min(u32::MAX as i64) as u32;
            }
            _ => return Err(format!("flag provided but not defined: -{name}")),
        }
    }
    Ok(o)
}
