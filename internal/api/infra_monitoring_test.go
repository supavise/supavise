package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/replicas"
)

func infraPath(q url.Values) string {
	return "/platform/projects/" + testRef + "/infra-monitoring?" + q.Encode()
}

func lagQuery(id string, attrs ...string) url.Values {
	q := url.Values{
		"startDate": {"2026-10-08T10:00:00Z"}, "endDate": {"2026-10-08T11:00:00Z"}, "interval": {"5m"},
		"attributes": {strings.Join(attrs, ",")},
	}
	if id != "" {
		q.Set("databaseIdentifier", id)
	}
	return q
}

// A request for the replication lag of a replica is answered from the controller's samples: the
// mean of each interval, as strings, in the shape Studio reads for one attribute.
func TestInfraMonitoringServesTheLagOfAReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	at := func(min, sec int) time.Time { return time.Date(2026, 10, 8, 10, min, sec, 0, time.UTC) }
	rf.svc.lag[id] = []replicas.LagPoint{
		{At: at(2, 0), Seconds: 1}, {At: at(4, 0), Seconds: 2}, // 10:00 bucket
		{At: at(5, 0), Seconds: 0.5},                                    // 10:05
		{At: at(12, 0), Seconds: -1},                                    // unknown lag is skipped
		{At: at(31, 30), Seconds: 7},                                    // 10:30
		{At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Seconds: 9}, // after the end
	}

	rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil)
	if rec.Code != 200 || rec.Header().Get("X-Supavise-Stub") != "" {
		t.Fatalf("%d %s stub=%q", rec.Code, rec.Body, rec.Header().Get("X-Supavise-Stub"))
	}
	var got struct {
		Data []map[string]string
		// The metadata Studio's chart scales by.
		YAxisLimit   float64 `json:"yAxisLimit"`
		Format       string  `json:"format"`
		Total        float64 `json:"total"`
		TotalAverage float64 `json:"totalAverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	var starts, values []string
	for _, p := range got.Data {
		starts, values = append(starts, p["period_start"]), append(values, p[lagAttribute])
	}
	if strings.Join(starts, " ") != "2026-10-08T10:00:00.000Z 2026-10-08T10:05:00.000Z 2026-10-08T10:30:00.000Z" || strings.Join(values, " ") != "1.5 0.5 7" {
		t.Fatalf("points = %v %v", starts, values)
	}
	if got.Format != "s" || got.YAxisLimit != 7 || got.Total != 9 || got.TotalAverage != 3 {
		t.Fatalf("meta = %+v", got)
	}
	if len(rf.svc.lagSince) != 1 || !rf.svc.lagSince[0].Equal(time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lag asked since %v", rf.svc.lagSince)
	}

	// Several attributes: the multi-attribute format, values by attribute, a series entry for each,
	// and an error entry for each metric Supavise does not collect for a replica, zero on every point.
	rec = rf.do("GET", infraPath(lagQuery(id, lagAttribute, "cpu_usage")), nil)
	var multi struct {
		Data []struct {
			PeriodStart string            `json:"period_start"`
			Values      map[string]string `json:"values"`
		}
		Series map[string]map[string]any
		Errors map[string]struct{ Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &multi); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
	if len(multi.Data) != 3 || multi.Data[0].Values[lagAttribute] != "1.5" || multi.Data[0].Values["cpu_usage"] != "0" ||
		multi.Series[lagAttribute]["format"] != "s" || multi.Series["cpu_usage"]["format"] != "%" || multi.Errors["cpu_usage"].Message == "" {
		t.Fatalf("multi = %+v", multi)
	}
	if _, bad := multi.Errors[lagAttribute]; bad {
		t.Fatalf("the lag is reported as an error: %+v", multi.Errors)
	}

	// attributes may be repeated, and the older single attribute works too.
	q := lagQuery(id)
	q.Add("attributes", lagAttribute)
	if rec := rf.do("GET", infraPath(q), nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"1.5"`) {
		t.Fatalf("repeated attributes: %d %s", rec.Code, rec.Body)
	}
	q = lagQuery(id)
	q.Set("attribute", lagAttribute)
	if rec := rf.do("GET", infraPath(q), nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"1.5"`) {
		t.Fatalf("attribute: %d %s", rec.Code, rec.Body)
	}
}

// The primary has no lag: the series is empty. A database of another project, or one that does
// not exist, is a 404.
func TestInfraMonitoringLagOfOtherDatabases(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	for _, id := range []string{"", testRef} {
		rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil)
		var got struct{ Data []any }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.Data == nil || len(got.Data) != 0 {
			t.Fatalf("primary %q: %d %s", id, rec.Code, rec.Body)
		}
	}
	for _, id := range []string{
		"bcdefghijklmnopqrstu-rr-eu-west-1-abcdef", // a replica of another project
		testRef + "-rr-eu-west-1-zzzzzz",           // this project's, but not a replica
		"nonsense",
	} {
		if rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil); rec.Code != 404 {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	for name, mod := range map[string]func(url.Values){
		"no dates":     func(q url.Values) { q.Del("startDate"); q.Del("endDate") },
		"reversed":     func(q url.Values) { q.Set("startDate", "2026-10-09T10:00:00Z") },
		"bad interval": func(q url.Values) { q.Set("interval", "7m") },
	} {
		q := lagQuery("", lagAttribute)
		mod(q)
		if rec := rf.do("GET", infraPath(q), nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// ---- the project's own series ------------------------------------------------------------------

var metricsNow = time.Date(2026, 10, 8, 11, 0, 30, 0, time.UTC)

// withMetrics gives the fixture's server a collector over a store in a temporary directory and a
// clock, and feeds it one sample a minute for the 30 minutes before metricsNow: feed sets the
// values of the sample for minute i.
func (f *fixture) withMetrics(t *testing.T, feed func(i int, s *metrics.Sample)) *metrics.Service {
	t.Helper()
	store := metrics.NewStore(t.TempDir())
	store.Now = func() time.Time { return metricsNow }
	svc := metrics.New(metrics.Options{Cfg: f.cfg, Projects: f.reg, Store: store, Now: func() time.Time { return metricsNow }})
	for i := 0; i < 30; i++ {
		s := metrics.NewSample(metricsNow.Add(time.Duration(i-30) * time.Minute).Truncate(time.Minute))
		feed(i, &s)
		if err := svc.Ingest(testRef, "First project", s); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.metrics = svc
	return svc
}

func healthyProject(i int, s *metrics.Sample) {
	s.Set(metrics.CPUUser, 20+float64(i%3))
	s.Set(metrics.CPUSystem, 5)
	s.Set(metrics.CPUBusy, 25+float64(i%3))
	s.Set(metrics.MemLimit, 1<<30)
	s.Set(metrics.MemUsed, 256<<20)
	s.Set(metrics.MemCache, 512<<20)
	s.Set(metrics.SwapUsed, 0)
	s.Set(metrics.Backends, 10)
	s.Set(metrics.MaxConnections, 60)
	s.Set(metrics.DBSize, 400<<20)
	s.Set(metrics.WALSize, 64<<20)
	s.Set(metrics.DiskUsed, 1<<30)
	s.Set(metrics.DiskSize, 8<<30)
	s.Set(metrics.DiskQuota, 1)
}

type multiBody struct {
	Data []struct {
		PeriodStart string            `json:"period_start"`
		Values      map[string]string `json:"values"`
	} `json:"data"`
	Series map[string]struct {
		YAxisLimit   float64 `json:"yAxisLimit"`
		Format       string  `json:"format"`
		Total        float64 `json:"total"`
		TotalAverage float64 `json:"totalAverage"`
	} `json:"series"`
	Errors map[string]struct{ Message string } `json:"errors"`
}

func metricsQuery(interval string, attrs ...string) url.Values {
	return url.Values{
		"startDate": {metricsNow.Add(-time.Hour).Format(time.RFC3339)}, "endDate": {metricsNow.Format(time.RFC3339)},
		"interval": {interval}, "attributes": {strings.Join(attrs, ",")},
	}
}

// The attributes of the Database report's memory and CPU charts come back in the multi-attribute
// shape with a series entry for each, whatever is measured, and the points hold what the collector
// stored: CPU as a share of the quota, memory in bytes against the project's limit.
func TestInfraMonitoringServesTheProjectsOwnSeries(t *testing.T) {
	f := newFixture(t)
	f.withMetrics(t, healthyProject)
	attrs := []string{"ram_usage_used", "ram_usage_cache_and_buffers", "ram_usage_free", "ram_usage_total", "ram_usage_swap",
		"cpu_usage_busy_system", "cpu_usage_busy_user", "cpu_usage_busy_iowait", "cpu_usage_busy_irqs", "cpu_usage_busy_other", "cpu_usage_busy_idle"}
	rec := f.do("GET", infraPath(metricsQuery("1m", attrs...)), nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got multiBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if len(got.Data) != 30 || got.Data[0].PeriodStart != "2026-10-08T10:30:00.000Z" || got.Data[29].PeriodStart != "2026-10-08T10:59:00.000Z" {
		t.Fatalf("%d points, first %v", len(got.Data), got.Data[0])
	}
	for _, a := range attrs {
		if _, ok := got.Series[a]; !ok {
			t.Errorf("no series for %s: Studio would draw nothing for the chart", a)
		}
		for _, p := range got.Data {
			if _, ok := p.Values[a]; !ok {
				t.Fatalf("%s has no value at %s", a, p.PeriodStart)
			}
		}
	}
	p0 := got.Data[0].Values
	if p0["ram_usage_used"] != "268435456" || p0["ram_usage_cache_and_buffers"] != "536870912" || p0["ram_usage_free"] != "268435456" ||
		p0["ram_usage_total"] != "1073741824" || p0["ram_usage_swap"] != "0" {
		t.Fatalf("memory = %v", p0)
	}
	if p0["cpu_usage_busy_user"] != "20" || p0["cpu_usage_busy_system"] != "5" || p0["cpu_usage_busy_idle"] != "75" ||
		p0["cpu_usage_busy_iowait"] != "0" || p0["cpu_usage_busy_irqs"] != "0" {
		t.Fatalf("cpu = %v", p0)
	}
	if m := got.Series["ram_usage_total"]; m.Format != "bytes" || m.YAxisLimit != 1<<30 {
		t.Fatalf("meta = %+v", m)
	}
	if m := got.Series["cpu_usage_busy_user"]; m.Format != "%" || m.YAxisLimit != 100 || m.TotalAverage != 21 {
		t.Fatalf("meta = %+v", m)
	}
	// The CPU split that no control group tells is zero without an error; nothing else is missing.
	if len(got.Errors) != 0 {
		t.Fatalf("errors = %v", got.Errors)
	}
}

// Attributes with no source (the network, a burstable disk, memory commitment, PgBouncer,
// Realtime) are zero on every point and listed in errors, which is what Studio's cards print.
func TestInfraMonitoringAttributesWithoutASourceAreZeroWithAReason(t *testing.T) {
	f := newFixture(t)
	f.withMetrics(t, healthyProject)
	none := []string{"network_receive_bytes", "network_transmit_bytes", "disk_io_budget", "disk_io_consumption", "ram_commit_used", "ram_commit_limit",
		"client_connections_pgbouncer", "realtime_channel_joins", "realtime_payload_size"}
	rec := f.do("GET", infraPath(metricsQuery("5m", append([]string{"pg_stat_database_num_backends"}, none...)...)), nil)
	var got multiBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
	if len(got.Data) != 6 {
		t.Fatalf("%d points in 30 minutes of 5, want 6", len(got.Data))
	}
	for _, a := range none {
		if got.Errors[a].Message == "" {
			t.Errorf("%s has no error entry", a)
		}
		if _, ok := got.Series[a]; !ok {
			t.Errorf("%s has no series entry", a)
		}
		if got.Data[0].Values[a] != "0" {
			t.Errorf("%s = %q, want 0", a, got.Data[0].Values[a])
		}
	}
	if _, bad := got.Errors["pg_stat_database_num_backends"]; bad || got.Data[0].Values["pg_stat_database_num_backends"] != "10" {
		t.Fatalf("connections: %v %v", got.Errors, got.Data[0].Values)
	}
}

// One attribute is answered in the single shape: the series' metadata on the response and the
// value on the point, as the Realtime report and the custom reports ask.
func TestInfraMonitoringSingleAttributeShape(t *testing.T) {
	f := newFixture(t)
	f.withMetrics(t, healthyProject)
	rec := f.do("GET", infraPath(metricsQuery("1h", "ram_usage")), nil)
	var got struct {
		Data         []map[string]string
		Format       string
		YAxisLimit   float64
		Total        float64
		TotalAverage float64
		Series       any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
	if got.Series != nil || got.Format != "%" || got.YAxisLimit != 100 || len(got.Data) != 1 || got.Data[0]["ram_usage"] != "25" || got.Data[0]["period_start"] != "2026-10-08T10:00:00.000Z" {
		t.Fatalf("%+v", got)
	}
	// A chart the project has no data for still has its metadata and an empty list.
	rec = f.do("GET", infraPath(metricsQuery("1h", "realtime_channel_joins")), nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.Format != "" || got.Data == nil {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
}

// A project nobody has sampled yet, or a node with no collector, has a series entry for each
// attribute and no points.
func TestInfraMonitoringWithoutHistory(t *testing.T) {
	f := newFixture(t)
	attrs := []string{"cpu_usage_busy_user", "network_receive_bytes", "realtime_channel_joins"}
	for _, withCollector := range []bool{false, true} {
		if withCollector {
			f.srv.metrics = metrics.New(metrics.Options{Cfg: f.cfg, Projects: f.reg, Store: metrics.NewStore(t.TempDir())})
		}
		rec := f.do("GET", infraPath(metricsQuery("1h", attrs...)), nil)
		var got multiBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
		}
		if got.Data == nil || len(got.Data) != 0 || len(got.Series) != 3 {
			t.Fatalf("collector %v: %s", withCollector, rec.Body)
		}
	}
}

// A name the catalog does not know is refused, as hosted refuses a name outside its enum, and a
// request that names thousands of them is refused before anything is built for them: the answer
// has a series entry per name and a value per name and row, so its size follows the request's.
func TestInfraMonitoringUnknownAttribute(t *testing.T) {
	repeated := func(q url.Values, names []string) url.Values { q["attributes"] = names; return q }
	f := newFixture(t)
	f.withMetrics(t, healthyProject)
	rec := f.do("GET", infraPath(metricsQuery("1m", "cpu_usage", "brand_new_metric")), nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "brand_new_metric") {
		t.Fatalf("one unknown name: %d %s", rec.Code, rec.Body)
	}
	names := make([]string, 0, 1000)
	for i := 0; i < cap(names); i++ {
		names = append(names, fmt.Sprintf("made_up_metric_%d", i))
	}
	for name, q := range map[string]url.Values{
		"repeated":      repeated(metricsQuery("1m"), names),
		"comma list":    metricsQuery("1m", strings.Join(names, ",")),
		"known + 1000":  metricsQuery("1m", append([]string{"cpu_usage"}, names...)...),
		"single (old)":  url.Values{"attribute": {names[0]}, "startDate": {"2026-10-08T00:00:00Z"}, "endDate": {"2026-10-08T01:00:00Z"}},
		"very long one": metricsQuery("1m", strings.Repeat("x", 5000)),
	} {
		rec := f.do("GET", infraPath(q), nil)
		if rec.Code != 400 || rec.Body.Len() > 1000 {
			t.Errorf("%s: %d, %d bytes: %.200s", name, rec.Code, rec.Body.Len(), rec.Body)
		}
	}
	// Every name the catalog has is accepted at once, and a repeated name counts once.
	all := metrics.Attributes()
	rec = f.do("GET", infraPath(metricsQuery("1m", append(all, all...)...)), nil)
	var got multiBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got.Series) != len(all) {
		t.Fatalf("every attribute: %d %v, %d series: %.300s", rec.Code, err, len(got.Series), rec.Body)
	}
}

func TestInfraMonitoringRequestErrors(t *testing.T) {
	f := newFixture(t)
	f.withMetrics(t, healthyProject)
	for name, mod := range map[string]func(url.Values){
		"no attribute": func(q url.Values) { q.Del("attributes") },
		"no dates":     func(q url.Values) { q.Del("startDate"); q.Del("endDate") },
		"bad interval": func(q url.Values) { q.Set("interval", "2m") },
	} {
		q := metricsQuery("1m", "cpu_usage")
		mod(q)
		if rec := f.do("GET", infraPath(q), nil); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/platform/projects/nonexistentprojectref/infra-monitoring?"+metricsQuery("1m", "cpu_usage").Encode(), nil); rec.Code != 404 {
		t.Errorf("unknown project: %d", rec.Code)
	}
	// The bracketed spelling of an array parameter works too.
	q := metricsQuery("1m")
	q.Del("attributes")
	q.Add("attributes[]", "cpu_usage")
	q.Add("attributes[]", "ram_usage")
	rec := f.do("GET", infraPath(q), nil)
	var got multiBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got.Series) != 2 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
}

// ---- resource warnings ---------------------------------------------------------------------

type warningBody struct {
	Project                    string  `json:"project"`
	IsReadonlyModeEnabled      bool    `json:"is_readonly_mode_enabled"`
	NeedPitr                   *string `json:"need_pitr"`
	DiskIoExhaustion           *string `json:"disk_io_exhaustion"`
	DiskSpaceExhaustion        *string `json:"disk_space_exhaustion"`
	CpuExhaustion              *string `json:"cpu_exhaustion"`
	MemoryAndSwapExhaustion    *string `json:"memory_and_swap_exhaustion"`
	AuthRateLimitExhaustion    *string `json:"auth_rate_limit_exhaustion"`
	AuthRestrictedEmailSending *string `json:"auth_restricted_email_sending"`
	AuthEmailOffender          *string `json:"auth_email_offender"`
}

func level(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

func TestResourceWarningsFromTheCollector(t *testing.T) {
	f := newFixture(t)
	f.withMetrics(t, func(i int, s *metrics.Sample) {
		healthyProject(i, s)
		s.Set(metrics.DiskUsed, 7.5*(1<<30)) // 94% of the 8 GiB quota
		s.Set(metrics.CPUBusy, 82)           // over the warning line for the last five minutes
	})
	rec := f.do("GET", "/platform/projects-resource-warnings?ref="+testRef, nil)
	var got []warningBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got) != 1 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
	w := got[0]
	if w.Project != testRef || w.IsReadonlyModeEnabled || level(w.DiskSpaceExhaustion) != "critical" || level(w.CpuExhaustion) != "warning" || level(w.MemoryAndSwapExhaustion) != "-" {
		t.Fatalf("%+v", w)
	}
	// Every field is present, null where Supavise has nothing to say: the spec requires them.
	body := rec.Body.String()
	for _, key := range []string{"need_pitr", "disk_io_exhaustion", "auth_rate_limit_exhaustion", "auth_restricted_email_sending", "auth_email_offender"} {
		if !strings.Contains(body, `"`+key+`":null`) {
			t.Errorf("%s is not null in %s", key, body)
		}
	}

	// By organization, and for every project the caller sees; an organization that is not theirs, or
	// a project that does not exist, has none.
	for path, want := range map[string]int{
		"/platform/projects-resource-warnings?slug=default":              1,
		"/platform/projects-resource-warnings":                           1,
		"/platform/projects-resource-warnings?slug=another-org":          0,
		"/platform/projects-resource-warnings?ref=nonexistentprojectref": 0,
		"/platform/projects-resource-warnings?ref=" + config.SystemRef:   0,
	} {
		rec := f.do("GET", path, nil)
		var ws []warningBody
		if err := json.Unmarshal(rec.Body.Bytes(), &ws); err != nil || rec.Code != 200 || len(ws) != want {
			t.Errorf("%s: %d %v %s", path, rec.Code, err, rec.Body)
		}
	}
}

// With no collector, or a project it has not sampled, nothing is wrong as far as anyone knows: a
// list entry with every field null.
func TestResourceWarningsWithoutData(t *testing.T) {
	f := newFixture(t)
	for _, src := range []MetricsSource{nil, metrics.New(metrics.Options{Cfg: f.cfg, Projects: f.reg, Store: metrics.NewStore(t.TempDir())})} {
		f.srv.metrics = src
		rec := f.do("GET", "/platform/projects-resource-warnings?ref="+testRef, nil)
		var got []warningBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got) != 1 {
			t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
		}
		w := got[0]
		if w.Project != testRef || w.DiskSpaceExhaustion != nil || w.CpuExhaustion != nil || w.MemoryAndSwapExhaustion != nil {
			t.Fatalf("%+v", w)
		}
	}
}
