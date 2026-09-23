package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// benchcli-rust keeps a copy of Ports, since it is not Go and cannot import
// this package; this is what keeps the two from drifting apart.
func TestRustClientPorts(t *testing.T) {
	src, err := os.ReadFile("../benchcli-rust/src/config.rs")
	if err != nil {
		t.Fatal(err)
	}
	for framework, portRange := range Ports {
		bounds := strings.Split(portRange, ":")
		want := fmt.Sprintf("%q => Some((%s, %s)),", framework, bounds[0], bounds[1])
		if !strings.Contains(string(src), want) {
			t.Errorf("benchcli-rust/src/config.rs has no %s", want)
		}
	}
	for framework, lang := range Langs {
		if want := fmt.Sprintf("%q => %q,", framework, lang); !strings.Contains(string(src), want) {
			t.Errorf("benchcli-rust/src/config.rs has no %s", want)
		}
	}
	names := append([]string(nil), FrameworkList...)
	sort.Strings(names)
	if want := fmt.Sprintf("EXPECTED_FRAMEWORKS: &str = %q;", strings.Join(names, ", ")); !strings.Contains(string(src), want) {
		t.Errorf("benchcli-rust/src/config.rs has no %s", want)
	}
}

// The h2 framework's server is Rust too, and has its port range as two
// constants of its own.
func TestRustServerPorts(t *testing.T) {
	src, err := os.ReadFile("../frameworks/h2/src/main.rs")
	if err != nil {
		t.Fatal(err)
	}
	bounds := strings.Split(Ports[H2], ":")
	for _, want := range []string{
		fmt.Sprintf("const FRAMEWORK: &str = %q;", H2),
		"const FIRST_PORT: u16 = " + bounds[0] + ";",
		"const LAST_PORT: u16 = " + bounds[1] + ";",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("frameworks/h2/src/main.rs has no %s", want)
		}
	}
}
