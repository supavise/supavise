package metrics

import (
	"math"
	"slices"
	"strconv"
	"time"
)

// LagAttribute is the replication lag of a read replica in seconds: the one attribute whose
// samples come from the replica controller (internal/replicas) and not from this package.
const LagAttribute = "physical_replication_lag_physical_replication_lag_seconds"

// agg accumulates the units of one bucket.
type agg struct {
	sum, n, peak [NumFields]float64
}

func newAgg() *agg {
	a := &agg{}
	for i := range a.peak {
		a.peak[i] = math.NaN()
	}
	return a
}

func (a *agg) add(u Unit) {
	w := float64(max(u.N, 1))
	for i := range u.Mean {
		if m := u.Mean[i]; !math.IsNaN(m) {
			a.sum[i] += m * w
			a.n[i] += w
		}
		if p := u.Max[i]; !math.IsNaN(p) && (math.IsNaN(a.peak[i]) || p > a.peak[i]) {
			a.peak[i] = p
		}
	}
}

func (a *agg) mean(f Field) (float64, bool) {
	if a.n[f] == 0 {
		return 0, false
	}
	return a.sum[f] / a.n[f], true
}

func (a *agg) max(f Field) (float64, bool) {
	if math.IsNaN(a.peak[f]) {
		return 0, false
	}
	return a.peak[f], true
}

// An attribute is one name of Studio's infra-monitoring query (the enum of the platform spec).
type attribute struct {
	// format is the unit Studio's chart reads from the series metadata: "%" makes it print a
	// percentage, "bytes" and "ms" and "s" name the unit, and "" is a plain number.
	format string
	// pct marks a percentage: the chart's axis runs to 100.
	pct bool
	// val computes the bucket's value; nil for an attribute with no source.
	val func(*agg) (float64, bool)
	// why is the text of the response's errors entry for an attribute with no source. Studio shows
	// it in place of the value in its summary cards and as the chart's error state.
	why string
	// zero marks an attribute with no source that is answered with zeros and no error: the metric
	// does not exist here (iowait of a control group), so zero is what it measures.
	zero bool
}

func mean(f Field) func(*agg) (float64, bool) {
	return func(a *agg) (float64, bool) { return a.mean(f) }
}
func peak(f Field) func(*agg) (float64, bool) {
	return func(a *agg) (float64, bool) { return a.max(f) }
}

// memPct is the share of the project's memory limit that num holds.
func memPct(num func(*agg) (float64, bool)) func(*agg) (float64, bool) {
	return func(a *agg) (float64, bool) {
		v, ok := num(a)
		lim, ok2 := a.mean(MemLimit)
		if !ok || !ok2 || lim <= 0 {
			return 0, false
		}
		return 100 * v / lim, true
	}
}

// memFree is the limit less what the units hold and what the page cache holds.
func memFree(a *agg) (float64, bool) {
	lim, ok1 := a.mean(MemLimit)
	used, ok2 := a.mean(MemUsed)
	cache, ok3 := a.mean(MemCache)
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return max(lim-used-cache, 0), true
}

// memAvailable is what the units could still take before the limit: the limit less what they hold,
// because the page cache is reclaimable.
func memAvailable(a *agg) (float64, bool) {
	lim, ok1 := a.mean(MemLimit)
	used, ok2 := a.mean(MemUsed)
	if !ok1 || !ok2 {
		return 0, false
	}
	return max(lim-used, 0), true
}

func cpuIdle(a *agg) (float64, bool) {
	b, ok := a.mean(CPUBusy)
	if !ok {
		return 0, false
	}
	return max(100-b, 0), true
}

func diskAvail(a *agg) (float64, bool) {
	size, ok1 := a.mean(DiskSize)
	used, ok2 := a.mean(DiskUsed)
	if !ok1 || !ok2 {
		return 0, false
	}
	return max(size-used, 0), true
}

// diskSystem is what the project's directory holds besides its databases and the WAL: the other
// services' state, logs, temporary files and the filesystem's own overhead.
func diskSystem(a *agg) (float64, bool) {
	used, ok1 := a.mean(DiskUsed)
	db, ok2 := a.mean(DBSize)
	wal, ok3 := a.mean(WALSize)
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return max(used-db-wal, 0), true
}

const (
	noNetwork   = "Supavise does not measure network traffic per project"
	noIOBudget  = "Supavise has no disk IO budget to measure: the volume is not a burstable cloud disk"
	noCommit    = "Supavise does not measure memory commitment per project"
	noPgBouncer = "Supavise pools connections with Supavisor, not PgBouncer"
	noRealtime  = "Supavise does not collect Realtime metrics"
	// NotCollectedForReplicas is the reason given for every attribute of a read replica but its lag.
	NotCollectedForReplicas = "Supavise does not collect this metric for read replicas"
)

// attributes is the catalog: every value of the platform spec's attribute enum. An attribute
// that is missing from a response blanks the whole chart in Studio, so a response has an entry
// for each name the request asks for (see Assemble). The test in this package compares the
// catalog with the vendored spec, so a refresh of the spec that adds a name fails it.
var attributes = map[string]attribute{
	// CPU, in percent of the project's CPU quota. The control group does not tell iowait, interrupt
	// time and the rest apart, so those series are zero and idle is what the busy ones leave.
	"cpu_usage":             {format: "%", pct: true, val: mean(CPUBusy)},
	"cpu_usage_busy_system": {format: "%", pct: true, val: mean(CPUSystem)},
	"cpu_usage_busy_user":   {format: "%", pct: true, val: mean(CPUUser)},
	"cpu_usage_busy_iowait": {format: "%", pct: true, zero: true},
	"cpu_usage_busy_irqs":   {format: "%", pct: true, zero: true},
	"cpu_usage_busy_other":  {format: "%", pct: true, zero: true},
	"cpu_usage_busy_idle":   {format: "%", pct: true, val: cpuIdle},
	"max_cpu_usage":         {format: "%", pct: true, val: peak(CPUBusy)},
	"avg_cpu_usage":         {format: "%", pct: true, val: mean(CPUBusy)},

	// Memory of the project's units against the project's MemoryMax.
	"ram_usage":                   {format: "%", pct: true, val: memPct(mean(MemUsed))},
	"ram_usage_total":             {format: "bytes", val: mean(MemLimit)},
	"ram_usage_available":         {format: "bytes", val: memAvailable},
	"ram_usage_used":              {format: "bytes", val: mean(MemUsed)},
	"ram_usage_free":              {format: "bytes", val: memFree},
	"ram_usage_cache_and_buffers": {format: "bytes", val: mean(MemCache)},
	"ram_usage_swap":              {format: "bytes", val: mean(SwapUsed)},
	"swap_usage":                  {format: "bytes", val: mean(SwapUsed)},
	"ram_commit_used":             {format: "bytes", why: noCommit},
	"ram_commit_limit":            {format: "bytes", why: noCommit},

	"network_receive_bytes":  {format: "bytes", why: noNetwork},
	"network_transmit_bytes": {format: "bytes", why: noNetwork},

	// Connections of the project's Postgres.
	"pg_stat_database_num_backends":             {val: mean(Backends)},
	"max_db_connections":                        {val: mean(MaxConnections)},
	"client_connections_postgres":               {val: mean(ConnPostgres)},
	"client_connections_authenticator":          {val: mean(ConnAuthenticator)},
	"client_connections_supabase_admin":         {val: mean(ConnAdmin)},
	"client_connections_supabase_auth_admin":    {val: mean(ConnAuthAdmin)},
	"client_connections_supabase_storage_admin": {val: mean(ConnStorageAdmin)},
	"client_connections_other":                  {val: mean(ConnOther)},
	"supavisor_connections_active":              {val: mean(ConnSupavisor)},
	"client_connections_pgbouncer":              {why: noPgBouncer},
	"pgbouncer_pools_client_active_connections": {why: noPgBouncer},

	// Disk.
	"pg_database_size":    {format: "bytes", val: mean(DBSize)},
	"disk_fs_size":        {format: "bytes", val: mean(DiskSize)},
	"disk_fs_used":        {format: "bytes", val: mean(DiskUsed)},
	"disk_fs_avail":       {format: "bytes", val: diskAvail},
	"disk_fs_used_wal":    {format: "bytes", val: mean(WALSize)},
	"disk_fs_used_system": {format: "bytes", val: diskSystem},
	"disk_iops_read":      {val: mean(IOReadOps)},
	"disk_iops_write":     {val: mean(IOWriteOps)},
	"disk_bytes_read":     {format: "bytes", val: mean(IOReadBytes)},
	"disk_bytes_written":  {format: "bytes", val: mean(IOWriteBytes)},
	"disk_io_budget":      {format: "%", pct: true, why: noIOBudget},
	"disk_io_consumption": {format: "%", pct: true, why: noIOBudget},
	"disk_io_usage":       {format: "%", pct: true, why: noIOBudget},

	// A replica's lag has its own source. The primary has none: no point carries it.
	LagAttribute: {format: "s"},

	"realtime_connections_connected":                  {why: noRealtime},
	"realtime_channel_joins":                          {why: noRealtime},
	"realtime_channel_events":                         {why: noRealtime},
	"realtime_channel_presence_events":                {why: noRealtime},
	"realtime_channel_db_events":                      {why: noRealtime},
	"realtime_authorization_rls_execution_time":       {format: "ms", why: noRealtime},
	"realtime_read_authorization_rls_execution_time":  {format: "ms", why: noRealtime},
	"realtime_write_authorization_rls_execution_time": {format: "ms", why: noRealtime},
	"realtime_payload_size":                           {format: "bytes", why: noRealtime},
	"realtime_replication_connection_lag":             {format: "ms", why: noRealtime},
	"realtime_sum_connections_connected":              {why: noRealtime},
}

// Known reports whether name is in the catalog.
func Known(name string) bool { _, ok := attributes[name]; return ok }

// Attributes lists the catalog's names, sorted.
func Attributes() []string {
	out := make([]string, 0, len(attributes))
	for k := range attributes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Row is the values of one bucket, by attribute. An attribute with no entry has no value in it.
type Row struct {
	At     time.Time
	Values map[string]float64
}

// Meta is what Studio's chart needs to scale and label one attribute.
type Meta struct {
	Format       string
	YAxisLimit   float64
	Total        float64
	TotalAverage float64
}

// Result answers a query: the buckets that hold a value of an attribute asked for, a Meta for
// every attribute asked for, and the reason for each attribute that has no source.
type Result struct {
	Rows   []Row
	Series map[string]Meta
	Errors map[string]string
}

// Assemble builds the result for attrs from rows, which it may change. Every attribute asked for
// gets a Meta, measured or not, known or not: Studio draws nothing for a chart whose series
// metadata is missing. An attribute with no source is zero in every row, and listed in Errors
// with its reason unless the metric is not applicable here (iowait). An attribute that is not in
// the catalog is treated the same way, with a reason that names it. missing, when not empty,
// replaces the reason of every attribute that no row carries a value of (the read replicas,
// which have nothing but their lag). The lag of a primary is the one attribute that has no point
// at all, and a row that holds none of the attributes asked for is dropped.
func Assemble(attrs []string, rows []Row, missing string) Result {
	res := Result{Series: map[string]Meta{}, Errors: map[string]string{}}
	for _, name := range attrs {
		a, known := attributes[name]
		has := false
		for _, r := range rows {
			if _, ok := r.Values[name]; ok {
				has = true
				break
			}
		}
		fill := false
		switch {
		case !known:
			res.Errors[name], fill = "Supavise does not know the attribute "+name, true
		case has:
		case missing != "":
			res.Errors[name], fill = missing, true
		case name == LagAttribute:
		case a.val == nil && a.why != "":
			res.Errors[name], fill = a.why, true
		case a.zero:
			fill = true
		}
		var sum, top float64
		n := 0
		for i := range rows {
			if rows[i].Values == nil {
				rows[i].Values = map[string]float64{}
			}
			if fill {
				rows[i].Values[name] = 0
			}
			if v, ok := rows[i].Values[name]; ok {
				sum += v
				top = max(top, v)
				n++
			}
		}
		m := Meta{Format: a.format, Total: round(sum, 3), YAxisLimit: max(1, math.Ceil(top))}
		if n > 0 {
			m.TotalAverage = round(sum/float64(n), 3)
		}
		if a.pct {
			m.YAxisLimit = 100
		}
		res.Series[name] = m
	}
	for _, r := range rows {
		for _, name := range attrs {
			if _, ok := r.Values[name]; ok {
				res.Rows = append(res.Rows, r)
				break
			}
		}
	}
	return res
}

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// FormatValue renders a value the way the response carries it: a string, whole for bytes and with
// at most three decimals otherwise.
func FormatValue(attr string, v float64) string {
	places := 3
	if attributes[attr].format == "bytes" {
		places = 0
	}
	return strconv.FormatFloat(round(v, places)+0, 'f', -1, 64)
}
