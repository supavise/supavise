package metrics

import (
	"math"
	"time"
)

// Field names one measured quantity of a Sample. The numbering is the on-disk layout of the
// store (store.go): a field is only ever added at the end, and NumFields changes with it, which
// makes the store start a project's file again (a change of layout loses the history, so avoid
// one when a derived value will do).
type Field int

const (
	// CPUUser, CPUSystem and CPUBusy are percent of the project's CPU quota (CPUQuota of its units;
	// the node's cores when there is none): the share of that CPU time the project's units spent in
	// user mode, in the kernel, and in both. CPUBusy never exceeds 100.
	CPUUser Field = iota
	CPUSystem
	CPUBusy
	// CPUThrottled is the share of the quota's time the project's threads waited because the quota
	// was spent, in percent (capped at 100).
	CPUThrottled
	// MemUsed is anon + shmem in bytes, summed over the project's units: what the units hold that
	// the kernel cannot drop (Postgres's shared_buffers is shmem).
	MemUsed
	// MemCache is the page cache that is not shmem, in bytes: file - shmem.
	MemCache
	// MemCurrent is memory.current in bytes, summed over the units.
	MemCurrent
	// MemLimit is the project's MemoryMax in bytes.
	MemLimit
	// SwapUsed is memory.swap.current in bytes, summed over the units.
	SwapUsed
	// OOMKills is how many processes of the project's units the kernel killed for lack of memory
	// since the previous sample.
	OOMKills

	// Connections of the project's Postgres, counted from pg_stat_activity: client backends by role,
	// those that are executing, and those that come from Supavisor.
	Backends
	ConnPostgres
	ConnAuthenticator
	ConnAdmin
	ConnAuthAdmin
	ConnStorageAdmin
	ConnOther
	ConnActive
	ConnSupavisor
	// MaxConnections is the cluster's max_connections.
	MaxConnections

	// DBSize is the sum of pg_database_size over every database, WALSize the size of pg_wal.
	DBSize
	WALSize
	// DiskUsed and DiskSize are the project's disk use and its limit in bytes: against its XFS
	// project quota where one is in force, against the volume otherwise.
	DiskUsed
	DiskSize
	// DiskQuota is 1 when DiskSize is an XFS project quota in force, 0 when it is the volume's size.
	DiskQuota

	// IOReadOps and IOWriteOps are Postgres's block reads and writes per second, IOReadBytes and
	// IOWriteBytes the bytes per second (writes include the WAL), from pg_stat_io and pg_stat_wal.
	IOReadOps
	IOWriteOps
	IOReadBytes
	IOWriteBytes

	// NumFields is the number of fields.
	NumFields
)

var fieldNames = [NumFields]string{
	"cpu_user", "cpu_system", "cpu_busy", "cpu_throttled",
	"mem_used", "mem_cache", "mem_current", "mem_limit", "swap_used", "oom_kills",
	"backends", "conn_postgres", "conn_authenticator", "conn_admin", "conn_auth_admin", "conn_storage_admin",
	"conn_other", "conn_active", "conn_supavisor", "max_connections",
	"db_size", "wal_size", "disk_used", "disk_size", "disk_quota",
	"io_read_ops", "io_write_ops", "io_read_bytes", "io_write_bytes",
}

// String is the field's name in logs and tests.
func (f Field) String() string {
	if f < 0 || f >= NumFields {
		return "unknown"
	}
	return fieldNames[f]
}

// Sample is what one project looked like in one minute. A value that could not be measured is NaN
// (a probe that failed, a rate with no earlier reading), and every consumer skips it.
type Sample struct {
	// At is the start of the minute the sample stands for, in UTC.
	At time.Time
	V  [NumFields]float64
}

// NewSample returns a sample for the minute of t with every value unknown.
func NewSample(t time.Time) Sample {
	s := Sample{At: t.UTC().Truncate(time.Minute)}
	for i := range s.V {
		s.V[i] = math.NaN()
	}
	return s
}

// Get returns a field and whether it was measured.
func (s Sample) Get(f Field) (float64, bool) {
	v := s.V[f]
	return v, !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Set stores v, or marks the field unknown when v is not a number.
func (s *Sample) Set(f Field, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = math.NaN()
	}
	s.V[f] = v
}

// minuteOf is the Unix minute a time falls in.
func minuteOf(t time.Time) int64 { return floorDiv(t.Unix(), 60) }

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
