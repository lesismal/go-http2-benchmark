//! The report files, field for field the JSON of benchcli-go/report's
//! ConnectionsReport, BenchEchoReport and BenchRateReport, so that the Go
//! client's report step (-r=true) reads this client's runs into the same
//! Summary and tables. BenchClient is "benchcli-rust", which the Client row
//! of the Summary shows as "rust".

use serde::Serialize;

pub const BENCH_CLIENT: &str = "benchcli-rust";
pub const BENCH_MULTIPLEX: &str = "BenchMultiplex";

#[derive(Serialize, Default)]
#[serde(rename_all = "PascalCase")]
pub struct ConnectionsReport {
    pub framework: String,
    pub bench_client: String,
    #[serde(rename = "TPS")]
    pub tps: i64,
    pub min: i64,
    pub avg: i64,
    pub max: i64,
    #[serde(rename = "TP50")]
    pub tp50: i64,
    #[serde(rename = "TP75")]
    pub tp75: i64,
    #[serde(rename = "TP90")]
    pub tp90: i64,
    #[serde(rename = "TP95")]
    pub tp95: i64,
    #[serde(rename = "TP99")]
    pub tp99: i64,
    pub used: i64,
    pub total: usize,
    pub success: i64,
    pub failed: i64,
    pub concurrency: usize,
    pub max_streams: i64,
}

#[derive(Serialize, Default)]
#[serde(rename_all = "PascalCase")]
pub struct BenchEchoReport {
    pub framework: String,
    pub bench_client: String,
    #[serde(rename = "TPS")]
    pub tps: i64,
    #[serde(rename = "EER")]
    pub eer: f64,
    pub min: i64,
    pub avg: i64,
    pub max: i64,
    #[serde(rename = "TP50")]
    pub tp50: i64,
    #[serde(rename = "TP75")]
    pub tp75: i64,
    #[serde(rename = "TP90")]
    pub tp90: i64,
    #[serde(rename = "TP95")]
    pub tp95: i64,
    #[serde(rename = "TP99")]
    pub tp99: i64,
    pub used: i64,
    pub total: usize,
    pub success: i64,
    pub failed: i64,
    pub conns: usize,
    pub concurrency: usize,
    pub streams: usize,
    pub payload: usize,
    #[serde(rename = "CPUMin")]
    pub cpu_min: f64,
    #[serde(rename = "CPUAvg")]
    pub cpu_avg: f64,
    #[serde(rename = "CPUMax")]
    pub cpu_max: f64,
    #[serde(rename = "MEMMin")]
    pub mem_min: u64,
    #[serde(rename = "MEMAvg")]
    pub mem_avg: u64,
    #[serde(rename = "MEMMax")]
    pub mem_max: u64,
}

#[derive(Serialize, Default)]
#[serde(rename_all = "PascalCase")]
pub struct BenchRateReport {
    pub framework: String,
    pub bench_client: String,
    pub duration: i64,
    #[serde(rename = "TPS")]
    pub tps: i64,
    #[serde(rename = "EchoEER")]
    pub echo_eer: f64,
    pub send_times: i64,
    pub send_bytes: i64,
    pub recv_times: i64,
    pub recv_bytes: i64,
    pub conns: usize,
    pub concurrency: usize,
    pub send_rate: usize,
    pub batch: usize,
    pub payload: usize,
    #[serde(rename = "CPUMin")]
    pub cpu_min: f64,
    #[serde(rename = "CPUAvg")]
    pub cpu_avg: f64,
    #[serde(rename = "CPUMax")]
    pub cpu_max: f64,
    #[serde(rename = "MEMMin")]
    pub mem_min: u64,
    #[serde(rename = "MEMAvg")]
    pub mem_avg: u64,
    #[serde(rename = "MEMMax")]
    pub mem_max: u64,
}

/// report.EER: throughput per percent of a CPU core, 0 without samples.
pub fn eer(throughput: f64, cpu_avg: f64) -> f64 {
    if !(cpu_avg > 0.0) || !throughput.is_finite() {
        return 0.0;
    }
    let e = throughput / cpu_avg;
    if e.is_finite() {
        e
    } else {
        0.0
    }
}

/// report.Filename.
pub fn filename(base: &str, preffix: &str, suffix: &str) -> String {
    format!("./output/report/{preffix}{base}{suffix}")
}

/// report.ToFile: the report's JSON, and its pprof profiles when it has any.
pub fn to_file<T: Serialize>(name: &str, r: &T, pprof: Option<(Vec<u8>, Vec<u8>)>, preffix: &str, suffix: &str) -> Result<(), String> {
    if let Some((cpu, mem)) = pprof {
        std::fs::write(filename(name, preffix, &format!("{suffix}.pprof.cpu")), cpu).map_err(|e| e.to_string())?;
        std::fs::write(filename(name, preffix, &format!("{suffix}.pprof.mem")), mem).map_err(|e| e.to_string())?;
    }
    let data = serde_json::to_vec(r).map_err(|e| e.to_string())?;
    std::fs::write(filename(name, preffix, &format!("{suffix}.json")), data).map_err(|e| e.to_string())
}

/// perf.I2TimeString.
pub fn time_string(ns: i64) -> String {
    if ns / 1_000_000_000 >= 1 {
        format!("{:.2}s", ns as f64 / 1e9)
    } else if ns / 1_000_000 >= 1 {
        format!("{:.2}ms", ns as f64 / 1e6)
    } else if ns / 1000 >= 1 {
        format!("{:.2}us", ns as f64 / 1e3)
    } else {
        format!("{ns}ns")
    }
}

/// perf.I2MemString.
pub fn mem_string(b: u64) -> String {
    let gb = b as f64 / (1u64 << 30) as f64;
    if gb >= 1.0 {
        return format!("{gb:.2}G");
    }
    let mb = b as f64 / (1u64 << 20) as f64;
    if mb >= 1.0 {
        return format!("{mb:.2}M");
    }
    format!("{:.2}K", b as f64 / 1024.0)
}

/// The block report.ObjString prints for a report as its benchmark finishes:
/// one aligned "name: value" line per field, the benchmark first.
pub fn console(bench: &str, fields: &[(&str, String)]) -> String {
    let width = fields.iter().map(|(k, _)| k.len()).chain(["BenchType".len()]).max().unwrap_or(0);
    let mut out = format!("{:width$}: {bench}\n", "BenchType");
    let lines: Vec<String> = fields.iter().map(|(k, v)| format!("{k:width$}: {v}")).collect();
    out += &lines.join("\n");
    out
}

impl ConnectionsReport {
    pub fn console(&self, tpn: bool) -> String {
        let mut f = vec![("Framework", self.framework.clone()), ("Lang", crate::config::lang(&self.framework).into()), ("Client", "rust".into()), ("TPS", self.tps.to_string())];
        if tpn {
            f.extend([("Min", time_string(self.min)), ("Avg", time_string(self.avg)), ("Max", time_string(self.max)),
                ("TP95", time_string(self.tp95)), ("TP99", time_string(self.tp99))]);
        }
        f.extend([("Used", time_string(self.used)), ("Total", self.total.to_string()), ("Success", self.success.to_string()),
            ("Failed", self.failed.to_string()), ("Concurrency", self.concurrency.to_string()), ("MaxStreams", self.max_streams.to_string())]);
        console("Connections", &f)
    }
}

impl BenchEchoReport {
    pub fn console(&self, tpn: bool) -> String {
        let mut f = vec![("Framework", self.framework.clone()), ("Lang", crate::config::lang(&self.framework).into()), ("Client", "rust".into()), ("TPS", self.tps.to_string()),
            ("EER", format!("{:.2}", self.eer))];
        if tpn {
            f.extend([("Min", time_string(self.min)), ("Avg", time_string(self.avg)), ("Max", time_string(self.max)),
                ("TP95", time_string(self.tp95)), ("TP99", time_string(self.tp99))]);
        }
        f.extend([("Used", time_string(self.used)), ("Total", self.total.to_string()), ("Success", self.success.to_string()),
            ("Failed", self.failed.to_string()), ("Conns", self.conns.to_string()), ("Concurrency", self.concurrency.to_string()),
            ("Streams", self.streams.to_string()), ("Payload", self.payload.to_string()),
            ("CPU Avg", format!("{:.2}%", self.cpu_avg)), ("CPU Max", format!("{:.2}%", self.cpu_max)),
            ("MEM Avg", mem_string(self.mem_avg)), ("MEM Max", mem_string(self.mem_max))]);
        console("BenchEcho", &f)
    }
}

impl BenchRateReport {
    pub fn console(&self) -> String {
        let f = vec![("Framework", self.framework.clone()), ("Lang", crate::config::lang(&self.framework).into()), ("Client", "rust".into()), ("Duration", time_string(self.duration)),
            ("TPS", self.tps.to_string()), ("EER", format!("{:.2}", self.echo_eer)), ("Req Sent", self.send_times.to_string()),
            ("Bytes Sent", mem_string(self.send_bytes as u64)), ("Resp Recv", self.recv_times.to_string()),
            ("Bytes Recv", mem_string(self.recv_bytes as u64)), ("Conns", self.conns.to_string()),
            ("Concurrency", self.concurrency.to_string()), ("SendRate", self.send_rate.to_string()), ("Batch", self.batch.to_string()),
            ("Payload", self.payload.to_string()), ("CPU Avg", format!("{:.2}%", self.cpu_avg)), ("CPU Max", format!("{:.2}%", self.cpu_max)),
            ("MEM Avg", mem_string(self.mem_avg)), ("MEM Max", mem_string(self.mem_max))];
        console(BENCH_MULTIPLEX, &f)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json_keys_match_the_go_reports() {
        let json = serde_json::to_string(&BenchEchoReport::default()).unwrap();
        for key in ["\"Framework\"", "\"BenchClient\"", "\"TPS\"", "\"EER\"", "\"TP95\"", "\"Conns\"", "\"Streams\"", "\"CPUAvg\"", "\"MEMMax\""] {
            assert!(json.contains(key), "{key} missing from {json}");
        }
        let json = serde_json::to_string(&BenchRateReport::default()).unwrap();
        for key in ["\"EchoEER\"", "\"SendTimes\"", "\"RecvBytes\"", "\"SendRate\"", "\"Batch\""] {
            assert!(json.contains(key), "{key} missing from {json}");
        }
        assert!(serde_json::to_string(&ConnectionsReport::default()).unwrap().contains("\"MaxStreams\""));
    }

    #[test]
    fn strings_follow_perf() {
        assert_eq!(time_string(1_500_000_000), "1.50s");
        assert_eq!(time_string(2_340_000), "2.34ms");
        assert_eq!(time_string(51_170), "51.17us");
        assert_eq!(time_string(999), "999ns");
        assert_eq!(mem_string(193 * (1 << 20) + (1 << 19)), "193.50M");
        assert_eq!(mem_string(3 * (1 << 30)), "3.00G");
        assert_eq!(eer(100.0, 0.0), 0.0);
    }
}
