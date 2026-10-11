package metrics

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

// specEnum reads the attribute enum of the platform spec the API serves from.
func specEnum(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../api/gen/specs/platform.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `json:"name"`
					Schema struct {
						Enum []string `json:"enum"`
					} `json:"schema"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Paths["/platform/projects/{ref}/infra-monitoring"].Get.Parameters {
		if p.Name == "attribute" {
			return p.Schema.Enum
		}
	}
	t.Fatal("the platform spec has no attribute parameter on infra-monitoring")
	return nil
}

// A name the spec lists and the catalog lacks would be answered without a series for it, which
// blanks the chart that asked. A refresh of the spec that adds a name fails here.
func TestCatalogCoversTheSpecEnum(t *testing.T) {
	enum := specEnum(t)
	if len(enum) < 50 {
		t.Fatalf("the spec lists %d attributes", len(enum))
	}
	for _, name := range enum {
		if !Known(name) {
			t.Errorf("the spec lists %q and the catalog does not", name)
		}
	}
	for _, name := range Attributes() {
		if !slices.Contains(enum, name) {
			t.Errorf("the catalog has %q, which the spec does not list", name)
		}
	}
}

func TestEveryAttributeHasASourceOrAReason(t *testing.T) {
	for name, a := range attributes {
		switch {
		case a.val != nil && (a.why != "" || a.zero):
			t.Errorf("%s has a source and also says it has none", name)
		case a.val == nil && a.why == "" && !a.zero && name != LagAttribute:
			t.Errorf("%s has no source and gives no reason", name)
		}
	}
}

func rows(vals ...map[string]float64) []Row {
	out := make([]Row, len(vals))
	for i, v := range vals {
		out[i] = Row{At: t0.Add(time.Duration(i) * time.Minute), Values: v}
	}
	return out
}

// Every attribute asked for has an entry in the series, a name the catalog has never heard of too.
func TestAssembleNeverOmitsASeries(t *testing.T) {
	attrs := []string{"cpu_usage", "network_receive_bytes", "cpu_usage_busy_iowait", "not_an_attribute", "client_connections_pgbouncer", LagAttribute}
	res := Assemble(attrs, rows(map[string]float64{"cpu_usage": 10}, map[string]float64{"cpu_usage": 30}), "")
	for _, a := range attrs {
		if _, ok := res.Series[a]; !ok {
			t.Errorf("no series for %s", a)
		}
	}
	if m := res.Series["cpu_usage"]; m.Format != "%" || m.YAxisLimit != 100 || m.Total != 40 || m.TotalAverage != 20 {
		t.Fatalf("cpu_usage meta = %+v", m)
	}
	// Without a source: zero in every row, and the reason, except where zero is the truth.
	for _, a := range []string{"network_receive_bytes", "client_connections_pgbouncer", "not_an_attribute"} {
		if res.Errors[a] == "" {
			t.Errorf("%s has no error entry", a)
		}
		for _, r := range res.Rows {
			if v, ok := r.Values[a]; !ok || v != 0 {
				t.Errorf("%s in %v = %v, %v; want a zero", a, r.At, v, ok)
			}
		}
	}
	for _, a := range []string{"cpu_usage_busy_iowait", "cpu_usage", LagAttribute} {
		if res.Errors[a] != "" {
			t.Errorf("%s is reported as an error: %s", a, res.Errors[a])
		}
	}
	if v, ok := res.Rows[0].Values["cpu_usage_busy_iowait"]; !ok || v != 0 {
		t.Errorf("iowait = %v, %v; want zero", v, ok)
	}
	// The lag of a primary has no point at all.
	if _, ok := res.Rows[0].Values[LagAttribute]; ok {
		t.Error("the primary has a lag point")
	}
	if m := res.Series[LagAttribute]; m.Format != "s" || m.YAxisLimit != 1 {
		t.Fatalf("lag meta = %+v", m)
	}
}

// A request for the lag of a primary alone has no rows: there is nothing to draw.
func TestAssembleDropsRowsWithNoValueAskedFor(t *testing.T) {
	res := Assemble([]string{LagAttribute}, rows(map[string]float64{"cpu_usage": 10}), "")
	if len(res.Rows) != 0 || len(res.Series) != 1 || len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
}

// A read replica: the lag is the only series, and everything else is zero with the reason.
func TestAssembleForAReplica(t *testing.T) {
	r := rows(map[string]float64{LagAttribute: 1.5}, map[string]float64{LagAttribute: 0.5})
	res := Assemble([]string{LagAttribute, "cpu_usage", "ram_usage"}, r, NotCollectedForReplicas)
	if res.Errors["cpu_usage"] != NotCollectedForReplicas || res.Errors["ram_usage"] != NotCollectedForReplicas || res.Errors[LagAttribute] != "" {
		t.Fatalf("errors = %v", res.Errors)
	}
	if m := res.Series[LagAttribute]; m.Total != 2 || m.TotalAverage != 1 || m.YAxisLimit != 2 {
		t.Fatalf("lag meta = %+v", m)
	}
	if res.Rows[1].Values["cpu_usage"] != 0 {
		t.Fatalf("%v", res.Rows[1].Values)
	}
}

// A replica that has not reported its lag yet still has the lag as the one metric collected for it.
func TestAssembleForAReplicaWithoutLagSamples(t *testing.T) {
	res := Assemble([]string{LagAttribute, "cpu_usage"}, nil, NotCollectedForReplicas)
	if msg, ok := res.Errors[LagAttribute]; ok || res.Errors["cpu_usage"] != NotCollectedForReplicas {
		t.Fatalf("errors = %v (lag error %q)", res.Errors, msg)
	}
	if _, ok := res.Series[LagAttribute]; !ok || len(res.Rows) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestFormatValue(t *testing.T) {
	for _, c := range []struct {
		attr string
		v    float64
		want string
	}{
		{"ram_usage_used", 1073741824.4, "1073741824"},
		{"cpu_usage", 12.34567, "12.346"},
		{"cpu_usage", 50, "50"},
		{"pg_stat_database_num_backends", 7.0004, "7"},
		{"cpu_usage", 0, "0"},
		{"cpu_usage", -0.0001, "0"},
	} {
		if got := FormatValue(c.attr, c.v); got != c.want {
			t.Errorf("FormatValue(%s, %v) = %q, want %q", c.attr, c.v, got, c.want)
		}
	}
}

// The percent of memory is the share of the limit, and idle is what busy leaves.
func TestDerivedAttributes(t *testing.T) {
	a := newAgg()
	var u Unit
	u.N = 1
	for i := range u.Mean {
		u.Mean[i], u.Max[i] = nan(), nan()
	}
	set := func(f Field, v float64) { u.Mean[f], u.Max[f] = v, v }
	set(MemLimit, 1000)
	set(MemUsed, 250)
	set(MemCache, 400)
	set(CPUBusy, 30)
	set(DiskSize, 100)
	set(DiskUsed, 60)
	set(DBSize, 30)
	set(WALSize, 10)
	a.add(u)
	for name, want := range map[string]float64{
		"ram_usage": 25, "ram_usage_free": 350, "ram_usage_available": 750, "cpu_usage_busy_idle": 70, "max_cpu_usage": 30,
		"disk_fs_avail": 40, "disk_fs_used_system": 20,
	} {
		got, ok := attributes[name].val(a)
		if !ok || got != want {
			t.Errorf("%s = %v (known %v), want %v", name, got, ok, want)
		}
	}
	// A bucket with a missing input has no value for what needs it.
	b := newAgg()
	if _, ok := attributes["ram_usage"].val(b); ok {
		t.Error("ram_usage of an empty bucket")
	}
}

func nan() float64 { return NewSample(t0).V[0] }
