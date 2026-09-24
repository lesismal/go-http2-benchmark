package report

import (
	"reflect"
	"strings"
)

// ProjectName is what the Summary table's first row says the run is: the
// benchmark it is from, so that a Summary copied out on its own, or read next
// to go-http1-benchmark's, still says which one it is.
const ProjectName = "GO-HTTP2-BENCHMARK"

// projectParameter is the name of that row. It is no report's field: every
// row of every report is from this project.
const projectParameter = "Project"

// SummaryParameters is the order the Summary table lists the run's parameters
// in, and what each one means: the names the report fields are tagged
// summary:"<name>" with. A tagged name missing from here still gets a row,
// after these, with no description.
var SummaryParameters = []SummaryParameter{
	{projectParameter, "The benchmark project these reports are from"},
	{"Client", "The benchmark client the load came from"},
	{"Conns", "HTTP/2 connections dialed (-c) and used by every benchmark"},
	{"Payload", "Request body size in bytes (-b), which the server echoes back"},
	{"Max Streams", "Streams the server lets one connection have open at once (servers' -maxstreams)"},
	{"Dial Concurrency", "Connections dialed at once in Connections (-dc)"},
	{"Echo Concurrency", "Requests in flight at once in BenchEcho, over all connections (-ec)"},
	{"Echo Streams", "Requests in flight at once on one connection in BenchEcho (-es)"},
	{"Echo Total", "Request/response round trips BenchEcho makes in all (-en)"},
	{"Rate Concurrency", "Writers sending BenchMultiplex's batches, over all connections (-rc)"},
	{"Rate Duration", "How long BenchMultiplex sends for (-rd)"},
	{"Rate SendRate", "Requests sent to each connection per second in BenchMultiplex (-rr)"},
	{"Rate Batch", "Requests, a stream each, sent to a connection at once in BenchMultiplex (-rpl)"},
}

// SummaryParameter is one row of the Summary table: the parameter's name and
// what it means.
type SummaryParameter struct {
	Name        string
	Description string
}

// summaryValue is one value a parameter took, and the frameworks it took it
// for, in the order they were read.
type summaryValue struct {
	value      string
	frameworks []string
}

// Summary is the table of the run's parameters, taken off the summary-tagged
// fields of every row of every report, after a first row naming the project. A parameter every row agrees on - the
// client, the payload, the concurrency a flag set - reads as that value. One
// the rows disagree on lists each value with the frameworks that had it:
//
//	20000 (fib, gin); 19998 (nethttp)
//
// The table is left-aligned, so that a long value reads from its start.
func Summary(tables ...[]Report) string {
	values := map[string][]summaryValue{}
	var names []string
	for _, reports := range tables {
		for _, r := range reports {
			value := reflect.Indirect(reflect.ValueOf(r))
			typ := value.Type()
			framework := value.FieldByName("Framework").String()
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				name := field.Tag.Get("summary")
				if name == "" {
					continue
				}
				if _, seen := values[name]; !seen {
					names = append(names, name)
				}
				values[name] = addSummaryValue(values[name], cellString(field, value.Field(i)), framework)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}

	rows := [][]string{{projectParameter, ProjectName, summaryDescription(projectParameter)}}
	for _, name := range summaryOrder(names) {
		rows = append(rows, []string{name, summaryString(values[name]), summaryDescription(name)})
	}
	return markdownTableAligned([]string{"Parameter", "Value", "Description"}, rows, true)
}

// summaryDescription is what SummaryParameters says a parameter means, or
// nothing for one it does not list.
func summaryDescription(name string) string {
	for _, p := range SummaryParameters {
		if p.Name == name {
			return p.Description
		}
	}
	return ""
}

func addSummaryValue(values []summaryValue, value, framework string) []summaryValue {
	for i := range values {
		if values[i].value == value {
			for _, f := range values[i].frameworks {
				if f == framework {
					return values
				}
			}
			values[i].frameworks = append(values[i].frameworks, framework)
			return values
		}
	}
	return append(values, summaryValue{value, []string{framework}})
}

func summaryString(values []summaryValue) string {
	if len(values) == 1 {
		return values[0].value
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = v.value + " (" + strings.Join(v.frameworks, ", ") + ")"
	}
	return strings.Join(parts, "; ")
}

// summaryOrder puts names in SummaryParameters order, and any it does not
// list after them in the order they were found.
func summaryOrder(names []string) []string {
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	ordered := make([]string, 0, len(names))
	for _, p := range SummaryParameters {
		if found[p.Name] {
			ordered = append(ordered, p.Name)
			delete(found, p.Name)
		}
	}
	for _, name := range names {
		if found[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered
}
