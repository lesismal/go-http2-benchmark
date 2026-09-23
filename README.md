# go-http2-benchmark

HTTP/2 server benchmark for Go frameworks, and Rust's h2 next to them, built
the same way as
[go-http1-benchmark](https://github.com/lesismal/go-http1-benchmark): the same
scripts, the same client structure, and the same report format.

| Framework | Package | Server |
| --- | --- | --- |
| `fib` | [github.com/lesismal/fib/go](https://github.com/lesismal/fib) | one fib engine bound to every port, HTTP/2 handler from `fib/go/http` with `HTTP2Only` |
| `gin` | [github.com/gin-gonic/gin](https://github.com/gin-gonic/gin) | `gin.New()` (no logger or recovery middleware) on `net/http`'s HTTP/2 |
| `h2` | [github.com/hyperium/h2](https://github.com/hyperium/h2) (Rust) | `h2::server` directly, not through hyper, on a tokio multi-thread runtime: a task per connection and one per stream ([`frameworks/h2`](frameworks/h2)) |
| `nethttp` | `net/http` | one `http.Server` per port, all sharing one `ServeMux`, HTTP/2 only |

Every server speaks HTTP/2 in cleartext with prior knowledge (h2c,
[RFC 9113 section 3.3](https://www.rfc-editor.org/rfc/rfc9113#section-3.3))
on its benchmark ports, and nothing else: no HTTP/1, no `Upgrade: h2c`, no TLS.
That measures each framework's HTTP/2 stack rather than a TLS library they
would all share. `nethttp` and `gin` get it from `http.Server.Protocols` with
only `UnencryptedHTTP2` set; `fib` from `HTTP2Only`; `h2` only ever speaks
HTTP/2. All four advertise the same `SETTINGS_MAX_CONCURRENT_STREAMS`, set with
the servers' `-maxstreams` (default 250, which is both net/http's and fib's
own default). `h2` is given net/http's 1MB receive windows for request bodies,
where its own default is RFC 9113's 64KB.

Every server answers `POST /echo` with the request body, byte for byte, with a
`content-length`. Each one listens on 50 ports (see `config.Ports`) so that a
client dialing a million connections from one address does not run out of
ephemeral ports toward any one port. `/init`, `/ps` and `/debug/pprof/` are on
a separate HTTP/1 `net/http` server on the port after the last benchmark port.
That way the framework being measured serves nothing but `/echo`, and control
requests never wait behind benchmark requests. The `h2` server has the same
`/init` and `/ps` on a small HTTP/1 server of its own threads, sampling its
own CPU (`getrusage`) and RSS; it has no pprof, so a client's `-ep`/`-rp`
fetches from it log a 404.

## Clients

Two clients measure the servers. They take the same flags and write the same
report files, and the Summary's `Client` row says which one a run used.
`BENCH_CLIENT` picks one (see [`script/config.sh`](script/config.sh)):

| `BENCH_CLIENT` | Client | How it speaks HTTP/2 |
| --- | --- | --- |
| `rust` (default) | [`benchcli-rust`](benchcli-rust) | [reqwest](https://github.com/seanmonstar/reqwest) 0.13 on hyper and h2, with `http2_prior_knowledge()`. Each connection is a `reqwest::Client` of its own, which keeps one HTTP/2 connection and multiplexes every request to it on that connection; hyper keeps to the server's stream limit itself |
| `go` | [`benchcli-go`](benchcli-go) | a small HTTP/2 client of its own ([`protocol`](benchcli-go/protocol)): requests HPACK-encoded once and written with only their stream ids patched in, `golang.org/x/net/http2`'s framer to read the responses, and its own flow control |

The report step, which turns the JSON files into the Summary and the tables,
is always the Go client's (`output/bin/bench.client -r=true`), so it is built
whichever client runs. Both clients open the stream and connection receive
windows to 1GB, so that flow control never holds a server back.

## What is measured

The client runs three benchmarks one after another on the same HTTP/2
connections:

| Benchmark | What it does | TPS is |
| --- | --- | --- |
| `Connections` | dials `-c` connections, `-dc` at a time: TCP, the preface and SETTINGS, and one `GET /echo` answered on each | connections established and answered per second |
| `BenchEcho` | `-en` request/response round trips: a `POST /echo` with `-b` random bytes on a stream of its own, and the response read back, with `-ec` requests in flight at once over all connections and at most `-es` on one | round trips per second |
| `BenchMultiplex` | multiplexing for `-rd` seconds: each connection gets `-rr` requests a second, sent in batches of `-rpl` streams at once (or, with `-rpl=0`, as many as fit in `-rbs` bytes) without waiting for earlier responses | responses read back per second |

`Connections` sends a request on each connection because a connection the
kernel accepted is not yet one the server is serving. That GET is the
equivalent of the WebSocket upgrade.

`BenchEcho` with `-es=1`, the default, has one request per connection in flight
at a time, as go-http1-benchmark's does, so the two repositories' numbers can
be read side by side. `-es` above 1 puts several streams on each connection at
once, which is what HTTP/2 is for; `-ec` is then capped at `-c` times `-es`:

```sh
bash script/benchmark.sh -c=1000 -ec=16000 -es=16   # 16 streams on each of 1000 connections
```

`BenchMultiplex` is go-http1-benchmark's `BenchPipeline`, with streams on one
connection where that has requests queued behind each other. It limits each
connection to four unanswered batches. When the server falls behind the rate,
the client skips that connection for a tick instead of queueing more requests,
so a slow server is measured by what it answered, not by how deep a backlog
the client built. The Go client also skips a connection that has no room for a
whole batch under the server's stream limit or send window. When the duration
is up, the client waits up to one more tick for the last batch, then counts.
`Rate Batch` in the Summary table is the number of requests sent together. Set
it with `-rpl`, which has to divide `-rr` so that the batch goes out a whole
number of times a second. The default, `-rpl=0`, uses the most requests that
fit in `-rbs` bytes (16KB) and divide `-rr`, which is 10 for the default 1KB
payload and 200 requests a second. The Go client writes a batch in one write;
reqwest has no such call, so the Rust client issues the batch's requests
together and hyper writes them as they come.

`-check=true` compares every response body with the request that was sent.

`EER` is throughput per percent of a CPU core: `TPS / CPU Avg`. The server's
CPU and memory are sampled every `-pi` ms. The Rust client always reads them
from the server's `/ps` route. The Go client's `-ps=auto` (the default)
samples the server process from the client side when it runs on the same
machine, and asks the server's `/ps` route when it does not; `local` and
`remote` force one or the other. A phase shorter than one sampling interval
has no samples, and its CPU, MEM and EER columns read 0. The client logs a
message when that happens.

## Run

Go 1.27 or later, and a recent stable Rust toolchain (cargo) for the `h2`
framework and the Rust client, which are one Cargo workspace at the
repository root. Without cargo, run the Go frameworks with the Go client:
`BENCH_CLIENT=go BENCH_FRAMEWORKS=fib,gin,nethttp`. From the repository root:

```sh
# all frameworks, 10k connections, 1k payload, the Rust client
bash script/benchmark.sh

# the Go client instead
BENCH_CLIENT=go bash script/benchmark.sh

# a subset, with client flags
BENCH_FRAMEWORKS=fib,nethttp bash script/benchmark.sh -c=10000 -en=2000000 -b=1024

# every framework through the Connections x BodySize x BenchTime matrix in script/config.sh
bash script/benchmarkN.sh

# 1m connections (needs the system settings below)
bash script/1m_conns_benchmark.sh
```

Change the defaults in [`script/config.sh`](script/config.sh). Reports are
written to `output/report`: one JSON file per framework and benchmark, plus
`Summary.md`, `Connections.md`, `BenchEcho.md` and `BenchMultiplex.md`. Server
logs are in `output/log`. `benchmark.sh` forwards only `-nodelay`,
`-reuseport`, `-b`, `-m` and `-maxstreams` to the servers. Every other flag
goes to the client; run `go run ./benchcli-go -h` for the list, or
`cargo run --release -p benchcli-rust -- -h`, which takes the same flags.

A million connections are a million `reqwest::Client`s for the Rust client,
each with a pool and a connection task of its own, so give the client node the
memory for that, or use `BENCH_CLIENT=go` there.

On Linux, `script/env.sh` pins the servers and the client to separate halves
of the CPUs with `taskset`, and splits by socket, NUMA node or core when
`lscpu` can tell them apart. Without `taskset` (macOS, for example), nothing is
pinned. The Rust client's tokio runtime starts one worker per CPU it is pinned
to.

### Docker

The Docker runner reads the CPU set and memory exposed by the Docker daemon,
uses about 75% of its CPUs and 80% of its memory, pins separate CPU groups for
the servers and the client, and copies reports, logs, console output and the
resource plan to `output/docker/<timestamp>`. The image carries the Rust
toolchain (copied from `rust:1-bookworm`) and the workspace's crates, fetched
and built when the image is built, since the benchmark itself runs with
`--network none`.

```sh
# short nethttp-only validation
bash script/docker_benchmark.sh --smoke

# full benchmark
bash script/docker_benchmark.sh

# focused run with explicit resource limits, measured by the Go client
BENCH_CLIENT=go BENCH_FRAMEWORKS=fib,gin \
DOCKER_BENCH_CPUS=8 DOCKER_BENCH_MEMORY=12g \
bash script/docker_benchmark.sh -c=10000 -en=2000000 -b=1024
```

Run `bash script/docker_benchmark.sh --help` for all overrides. From mainland
China, use `script/docker_benchmark_cn.sh` instead. It takes the same options
and builds the image from mirrors (DaoCloud for Docker Hub, Aliyun for apt,
goproxy.cn for Go modules, rsproxy.cn for crates). Only the build downloads
anything, and the benchmark itself runs with `--network none`.

The [Docker benchmark workflow](.github/workflows/docker-benchmark.yml) runs
the same script on every push to `main`, or by hand from the Actions tab
(where the client can be chosen too), and writes the tables to the job
summary. The container gets 8 CPUs when the runner has at least 8, and 4
otherwise, so every CI run is one of those two sizes. The standard
GitHub-hosted runner has 4 CPUs; to get 8, set the repository variable
`DOCKER_BENCH_RUNNER` to the label of a larger runner. A runner with fewer than
4 CPUs fails the job.

## Report format

The reports have the same layout as go-http1-benchmark's: a Summary table of
the run's parameters, each with a description of what it means and the flag
that sets it, then one table per benchmark.

- Every table's first two columns are `Framework` and `Lang`, the language
  the framework's server is written in (`go`, `rust`), from `config.Langs`.
  It is worked out from the framework's name when the tables are made, so it
  is not in the JSON files, and a new framework needs its language there.
- Rows are ranked best first by `TPS`. In `BenchEcho` and `BenchMultiplex`, a
  tie is broken by `EER`. The ranked columns carry `[↓1]` and `[↓2]` in their
  titles.
- Every ranked column shows each row's share of the best value in that
  column, floored so that only the best row reads `100%`.
- Parameters shared by every row (`Client`, `Conns`, `Payload`, `Max Streams`,
  each benchmark's concurrency, `Echo Streams`, `Echo Total`, `Rate Duration`,
  `Rate SendRate`, `Rate Batch`) are in the Summary table instead of the
  columns. A parameter the rows disagree on lists each value with its
  frameworks, for example `20000 (fib); 19998 (nethttp)`.
- `Max Streams` is the `SETTINGS_MAX_CONCURRENT_STREAMS` the servers sent, as
  the Go client reads it off each connection; hyper keeps it to itself, so the
  Rust client records the `-maxstreams` the servers were started with.
- The JSON files keep every field, including `TP50`, `TP75`, `TP90`,
  `CPU Min` and `MEM Min`, which the tables leave out.

`BENCH_REPORT_SORT=framework` (or `-sort=framework`) keeps
`config.FrameworkList` order instead, so that a framework is on the same row
in every table and across runs. Neither order changes the numbers, so
re-running the report step alone is enough:

```sh
bash script/report.sh -sort=framework
```

## Two nodes

To run the servers and the client on separate machines, set `BENCH_ROLE` on
each and give the client side the server's address. The servers bind every
interface, so nothing needs configuring on their side.

```sh
# On the server node: builds the servers, starts them, leaves them running.
BENCH_ROLE=server bash script/benchmark.sh

# On the client node: builds the client, runs it against the servers, reports.
BENCH_ROLE=client BENCH_SERVER_HOST=10.0.0.2 bash script/benchmark.sh

# Back on the server node, when the run is over.
bash script/killall.sh
```

A node that runs one half gets the whole machine. The client cannot stop
servers it did not start, so every server stays up for the whole run. Use
`BENCH_FRAMEWORKS` to run a subset if the idle servers would disturb the one
being measured. The CPU and MEM columns come from the servers' own `/ps`
route, because the client cannot see the server process from another machine.

## Before running the test

Set the system limits on every machine the benchmark runs on. The client node
needs the port range and file descriptor limits as much as the server does:

```sh
sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sysctl -w fs.file-max=2000500
sysctl -w fs.nr_open=2000500
sysctl -w net.nf_conntrack_max=2000500
ulimit -n 2000500
sysctl -w net.ipv4.tcp_mem='131072  262144  524288'
sysctl -w net.ipv4.tcp_rmem='8760  256960  4088000'
sysctl -w net.ipv4.tcp_wmem='8760  256960  4088000'
sysctl -w net.core.rmem_max=16384
sysctl -w net.core.wmem_max=16384
sysctl -w net.core.somaxconn=2048
sysctl -w net.ipv4.tcp_max_syn_backlog=2048
sysctl -w /proc/sys/net/core/netdev_max_backlog=2048
sysctl -w net.ipv4.tcp_tw_reuse=1
```

On macOS, the kernel's socket buffer memory (`kern.ipc.nmbclusters`) runs out
long before the descriptors do: at a few thousand connections under
`BenchMultiplex`'s default rate, `netstat -m` counts "requests for memory
denied" and the kernel resets connections under both clients. The Go client
stops using a connection that broke; reqwest redials it, and the requests on
it fail, which the Rust client logs as `BenchMultiplex errors`. Either way
`Resp Recv` falls far below `Req Sent`, and those numbers measure the kernel,
not the server. Keep a macOS run to around a thousand connections, or run on
Linux or in Docker.

## Sample results

None are published yet. `bash script/docker_benchmark.sh` writes a run's
tables, with the resources it had, to `output/docker/<timestamp>`; the Docker
benchmark workflow writes them to its job summary.
