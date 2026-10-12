package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/registry"
)

// sized is a MetricsSource that holds the daily mean database size, in bytes, of each project.
type sized struct{ days map[string][]metrics.Row }

func (m *sized) Query(ref string, attrs []string, from, to time.Time, _ time.Duration) (metrics.Result, error) {
	var rows []metrics.Row
	for _, r := range m.days[ref] {
		if !r.At.Before(from.Truncate(24*time.Hour)) && r.At.Before(to) {
			rows = append(rows, r)
		}
	}
	return metrics.Result{Rows: rows}, nil
}
func (*sized) Warnings(string) (metrics.Warnings, bool) { return metrics.Warnings{}, false }
func (*sized) Forget(string)                            {}

const (
	usageRefTwo   = "bbbbbbbbbbbbbbbbbbbb"
	usageRefOther = "cccccccccccccccccccc"
	sizeGiB       = float64(1 << 30)
	// decimalGB is the unit of the "usage" figure Studio prints as GB.
	decimalGB = 1e9
)

func day(d int, bytes float64) metrics.Row {
	return metrics.Row{At: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC), Values: map[string]float64{"pg_database_size": bytes}}
}

// orgUsageFixture has the default organization with two projects, another organization with one, and a
// collector that holds database sizes for all three and for the system project.
func orgUsageFixture(t *testing.T, mx MetricsSource) *fixture {
	t.Helper()
	now := time.Date(2026, 10, 10, 12, 30, 20, 0, time.UTC)
	f := newFixture(t)
	other, err := f.reg.CreateOrganization(context.Background(), "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	f.mgr.addProject(t, usageRefTwo, "Second project", f.org.ID, registry.StatusActiveHealthy)
	f.mgr.addProject(t, usageRefOther, "Elsewhere", other.ID, registry.StatusActiveHealthy)
	srv, err := NewServer(Deps{Registry: f.reg, Secrets: f.mgr.sec, Manager: f.mgr, Config: f.cfg, PGMetaURL: f.meta.URL,
		Metrics: mx, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	f.ownerOn(srv)
	return f
}

func sizes() *sized {
	return &sized{days: map[string][]metrics.Row{
		testRef:       {day(8, 4*sizeGiB), day(9, 6*sizeGiB), day(10, 5*sizeGiB)},
		usageRefTwo:   {day(9, 2*sizeGiB), day(10, 2*sizeGiB)},
		usageRefOther: {day(10, 100*sizeGiB)},
		"system":      {day(10, 50*sizeGiB)},
	}}
}

type usageBody struct {
	UsageBillingEnabled bool `json:"usage_billing_enabled"`
	Usages              []struct {
		Metric            string  `json:"metric"`
		Usage             float64 `json:"usage"`
		UsageOriginal     float64 `json:"usage_original"`
		Cost              float64 `json:"cost"`
		AvailableInPlan   bool    `json:"available_in_plan"`
		Capped            bool    `json:"capped"`
		Unlimited         bool    `json:"unlimited"`
		PricingStrategy   string  `json:"pricing_strategy"`
		ProjectAllocation []struct {
			Ref   string  `json:"ref"`
			Name  string  `json:"name"`
			Usage float64 `json:"usage"`
		} `json:"project_allocations"`
	} `json:"usages"`
}

// nearly compares a figure of the answer, which is rounded to six decimals.
func nearly(got, want float64) bool { return math.Abs(got-want) < 1e-6 }

func decodeUsage(t *testing.T, rec interface {
	Bytes() []byte
}) usageBody {
	t.Helper()
	var b usageBody
	if err := json.Unmarshal(rec.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The summary reports what the node measures, the size of the organization's databases, as unlimited
// (a node has no quota), and nothing it does not measure.
func TestOrgUsageReportsDatabaseSize(t *testing.T) {
	f := orgUsageFixture(t, sizes())
	rec := f.do("GET", "/platform/organizations/default/usage", nil)
	if rec.Code != 200 || rec.Header().Get("X-Supavise-Stub") != "" {
		t.Fatalf("%d %s stub=%q", rec.Code, rec.Body, rec.Header().Get("X-Supavise-Stub"))
	}
	b := decodeUsage(t, rec.Body)
	if b.UsageBillingEnabled || len(b.Usages) != 1 {
		t.Fatalf("usage = %s", rec.Body)
	}
	u := b.Usages[0]
	// 5 GiB (the mean of 4, 6 and 5) and 2 GiB: 7 GiB, which is 7.516 GB. The other organization's and the
	// system project's databases are not counted. Only "usage" is in GB: "usage_original" and each
	// allocation are in bytes, as Studio's tooltip (formatUsage divides by 1e9) and project rows read them.
	if u.Metric != "DATABASE_SIZE" || !nearly(u.Usage, 7*sizeGiB/decimalGB) || u.UsageOriginal != 7*sizeGiB || u.Cost != 0 || !u.AvailableInPlan || u.Capped || !u.Unlimited || u.PricingStrategy != "NONE" {
		t.Errorf("entry = %+v", u)
	}
	if len(u.ProjectAllocation) != 2 || u.ProjectAllocation[0].Ref != testRef || u.ProjectAllocation[0].Name != "First project" || u.ProjectAllocation[0].Usage != 5*sizeGiB ||
		u.ProjectAllocation[1].Ref != usageRefTwo || u.ProjectAllocation[1].Usage != 2*sizeGiB {
		t.Errorf("allocations = %+v", u.ProjectAllocation)
	}
	// The tooltip's rows and its Total row use one unit, so the rows add up to usage_original.
	var sum float64
	for _, a := range u.ProjectAllocation {
		sum += a.Usage
	}
	if sum != u.UsageOriginal {
		t.Errorf("allocations add up to %v bytes, usage_original is %v", sum, u.UsageOriginal)
	}

	// One project of the organization.
	rec = f.do("GET", "/platform/organizations/default/usage?project_ref="+usageRefTwo, nil)
	b = decodeUsage(t, rec.Body)
	if rec.Code != 200 || len(b.Usages) != 1 || !nearly(b.Usages[0].Usage, 2*sizeGiB/decimalGB) || len(b.Usages[0].ProjectAllocation) != 1 || b.Usages[0].ProjectAllocation[0].Usage != 2*sizeGiB {
		t.Errorf("one project: %d %s", rec.Code, rec.Body)
	}
	// A range: only the days inside it, as Studio sends it for a custom period.
	rec = f.do("GET", "/platform/organizations/default/usage?start=2026-10-09T00:00:00.000Z&end=2026-10-10T00:00:00.000Z", nil)
	b = decodeUsage(t, rec.Body)
	if rec.Code != 200 || len(b.Usages) != 1 || !nearly(b.Usages[0].Usage, 8*sizeGiB/decimalGB) {
		t.Errorf("range: %d %s, want 6 + 2 GiB", rec.Code, rec.Body)
	}
}

// A project of another organization, the system project and an unknown ref are not this organization's
// usage; a malformed range is a 400.
func TestOrgUsageRefusesWhatIsNotTheOrganizations(t *testing.T) {
	f := orgUsageFixture(t, sizes())
	for _, ref := range []string{usageRefOther, "system", "nosuchprojectrefxxxx"} {
		if rec := f.do("GET", "/platform/organizations/default/usage?project_ref="+ref, nil); rec.Code != http.StatusNotFound {
			t.Errorf("project_ref %s: %d %s", ref, rec.Code, rec.Body)
		}
	}
	if rec := f.do("GET", "/platform/organizations/nosuchorg/usage", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown organization: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"start=yesterday", "end=tomorrow", "start=2026-10-10T00:00:00Z&end=2026-10-01T00:00:00Z", "start=2024-01-01T00:00:00Z&end=2026-10-01T00:00:00Z"} {
		for _, path := range []string{"/usage", "/usage/daily"} {
			if rec := f.do("GET", "/platform/organizations/default"+path+"?"+q, nil); rec.Code != http.StatusBadRequest {
				t.Errorf("%s?%s: %d %s", path, q, rec.Code, rec.Body)
			}
		}
	}
}

// Without a collector, or before it has a sample, the organization has no usage to report: the page
// shows its sections without a figure, which says less than a zero.
func TestOrgUsageWithoutMeasurements(t *testing.T) {
	for name, mx := range map[string]MetricsSource{"no collector": nil, "no samples": &sized{}} {
		f := orgUsageFixture(t, mx)
		rec := f.do("GET", "/platform/organizations/default/usage", nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"usage_billing_enabled":false,"usages":[]}` {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		rec = f.do("GET", "/platform/organizations/default/usage/daily?start=2026-10-01T00:00:00Z&end=2026-10-31T00:00:00Z", nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"usages":[]}` {
			t.Errorf("%s daily: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// The daily answer has one point a day, summed over the projects, with the date as Studio's chart groups
// it and the size in bytes.
func TestOrgUsageDaily(t *testing.T) {
	f := orgUsageFixture(t, sizes())
	rec := f.do("GET", "/platform/organizations/default/usage/daily?start=2026-10-01T00:00:00Z&end=2026-10-11T00:00:00Z", nil)
	if rec.Code != 200 || rec.Header().Get("X-Supavise-Stub") != "" {
		t.Fatalf("%d %s stub=%q", rec.Code, rec.Body, rec.Header().Get("X-Supavise-Stub"))
	}
	var got struct {
		Usages []struct {
			Date          string          `json:"date"`
			Metric        string          `json:"metric"`
			Usage         float64         `json:"usage"`
			UsageOriginal float64         `json:"usage_original"`
			Breakdown     json.RawMessage `json:"breakdown"`
		} `json:"usages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		date  string
		bytes float64
	}{{"2026-10-08", 4 * sizeGiB}, {"2026-10-09", 8 * sizeGiB}, {"2026-10-10", 7 * sizeGiB}}
	if len(got.Usages) != len(want) {
		t.Fatalf("points = %s", rec.Body)
	}
	for i, w := range want {
		g := got.Usages[i]
		if g.Date != w.date || g.Metric != "DATABASE_SIZE" || g.UsageOriginal != w.bytes || !nearly(g.Usage, w.bytes/decimalGB) || string(g.Breakdown) != "null" {
			t.Errorf("point %d = %+v, want %s %v bytes", i, g, w.date, w.bytes)
		}
	}
	// One project, and the default range when the query has none (the month of the server's clock).
	rec = f.do("GET", "/platform/organizations/default/usage/daily?project_ref="+testRef, nil)
	if rec.Code != 200 || strings.Count(rec.Body.String(), `"date"`) != 3 {
		t.Errorf("one project: %d %s", rec.Code, rec.Body)
	}
}

// The Usage page prints the subscription's period: the calendar month of the node's clock, in UTC, and
// not the epoch of an empty answer.
func TestSubscriptionHasTheCurrentMonthAsItsPeriod(t *testing.T) {
	f := orgUsageFixture(t, nil)
	rec := f.do("GET", "/platform/organizations/default/billing/subscription", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct {
		Start int64 `json:"current_period_start"`
		End   int64 `json:"current_period_end"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix(); got.Start != want {
		t.Errorf("period start %d, want %d", got.Start, want)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix(); got.End != want {
		t.Errorf("period end %d, want %d", got.End, want)
	}
}

func TestBillingPeriod(t *testing.T) {
	s, e := billingPeriod(time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("x", -8*3600)))
	if !s.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || !e.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period %v - %v: 23:59 in UTC-8 on 31 December is already January in UTC", s, e)
	}
}
