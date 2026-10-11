// Package svclog reads the logs of one project from the journal: `supavise logs <ref>`.
//
// A node keeps no log table for the dashboard. Every unit writes to journald, and the unit name
// selects the log (docs/design.md section 2). A project owns three units (Postgres, GoTrue,
// PostgREST); Realtime, Storage, Supavisor and the Edge Runtime serve every project, so their
// lines are shown only when they mention the project's ref, which they carry as the tenant.
package svclog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/supavise/supavise/internal/config"
)

// Source is one log of a project.
type Source struct {
	Name string // the name --service takes and the label of each line
	Unit string // the systemd unit
	// Shared marks a unit that serves every project: only the lines that mention the ref are the
	// project's.
	Shared bool
}

// source describes a Source before the ref is known.
type source struct {
	name    string
	aliases []string
	svc     string // config.Svc*
	shared  bool
	// system is whether the system project has the service.
	system bool
}

var sources = []source{
	{name: "postgres", aliases: []string{"db", "database"}, svc: config.SvcPostgres, system: true},
	{name: "auth", aliases: []string{"gotrue"}, svc: config.SvcGoTrue, system: true},
	{name: "rest", aliases: []string{"postgrest", "api"}, svc: config.SvcPostgREST},
	{name: "realtime", svc: config.SvcRealtime, shared: true},
	{name: "storage", svc: config.SvcStorage, shared: true},
	{name: "pooler", aliases: []string{"supavisor"}, svc: config.SvcSupavisor, shared: true},
	{name: "functions", aliases: []string{"edge-runtime", "edge-functions"}, svc: config.SvcEdgeRuntime, shared: true},
}

// Names are the values of --service, in the order they are shown.
func Names() []string {
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = s.name
	}
	return out
}

var refRe = regexp.MustCompile(`^[a-z]{20}$`)

// Resolve returns the logs to read for project ref: the one service named, or, with an empty
// service, all of them. The system project has no PostgREST and no tenants.
func Resolve(ref, service string) ([]Source, error) {
	if ref != config.SystemRef && !refRe.MatchString(ref) {
		return nil, fmt.Errorf("%q is not a project ref (20 lowercase letters, or %q)", ref, config.SystemRef)
	}
	var out []Source
	for _, s := range sources {
		if service != "" && !s.is(service) {
			continue
		}
		if ref == config.SystemRef && !s.system {
			if service != "" {
				return nil, fmt.Errorf("the system project has no %s log; use --service postgres or auth", s.name)
			}
			continue
		}
		unitRef := ref
		if s.shared {
			unitRef = ""
		}
		out = append(out, Source{Name: s.name, Unit: config.UnitName(s.svc, unitRef), Shared: s.shared})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("unknown service %q; choose one of %s", service, strings.Join(Names(), ", "))
	}
	return out, nil
}

func (s source) is(name string) bool {
	name = strings.ToLower(name)
	if name == s.name {
		return true
	}
	for _, a := range s.aliases {
		if name == a {
			return true
		}
	}
	return false
}

// Journal starts journalctl. The returned reader is the program's standard output; Close ends the
// program and reports a failure of it (a non-zero exit, with what it printed to standard error).
type Journal interface {
	Start(ctx context.Context, args []string) (io.ReadCloser, error)
}

// ExecJournal runs the journalctl program.
type ExecJournal struct{ Program string }

// Start implements Journal.
func (j ExecJournal) Start(ctx context.Context, args []string) (io.ReadCloser, error) {
	prog := j.Program
	if prog == "" {
		prog = "journalctl"
	}
	ctx, cancel := context.WithCancel(ctx)
	c := exec.CommandContext(ctx, prog, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := c.Start(); err != nil {
		cancel()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s not found: this command reads the systemd journal, so it needs a server that runs Supavise under systemd", prog)
		}
		return nil, err
	}
	return &proc{ReadCloser: out, c: c, cancel: cancel, stderr: &stderr}, nil
}

type proc struct {
	io.ReadCloser
	c      *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
	eof    atomic.Bool
	once   sync.Once
	err    error
}

// Read notes the end of the stream: a program whose output ended is exiting on its own.
func (p *proc) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if errors.Is(err, io.EOF) {
		p.eof.Store(true)
	}
	return n, err
}

// Close waits for the program. One that ended its output on its own and failed is an error (with
// what it printed to standard error); one that is still running, because the reader stopped early
// or follow mode ended, is stopped and is not.
func (p *proc) Close() error {
	p.once.Do(func() {
		stopped := !p.eof.Load()
		if stopped {
			p.cancel()
		}
		werr := p.c.Wait()
		p.cancel()
		if werr != nil && !stopped {
			msg := strings.TrimSpace(p.stderr.String())
			if msg == "" {
				msg = werr.Error()
			}
			p.err = errors.New("journalctl: " + msg)
		}
	})
	return p.err
}

// Options say what to print.
type Options struct {
	Ref     string
	Service string // "" for every log of the project
	Lines   int    // the most recent lines to start with
	Follow  bool
	Since   string // "30m", "2h", "1d" or a time; "" for no limit
	Out     io.Writer
	Err     io.Writer // notes and hints
	Journal Journal
	// Tail, when set, replaces the journal: it returns the end of a unit's log file (the exec
	// backend of development and tests, which keeps no journal). Follow and Since are refused.
	Tail func(unit string, lines int) string
	Now  func() time.Time
	// Root is whether the process can read the whole system journal; without it an empty result
	// carries a hint.
	Root bool
}

// Run prints the log lines of the project. The most recent Lines lines of each log are merged by
// time and the last Lines of them are printed; with Follow, new lines are printed as they arrive
// until ctx ends.
func Run(ctx context.Context, o Options) error {
	if o.Lines < 0 {
		return errors.New("-n must not be negative")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	srcs, err := Resolve(o.Ref, o.Service)
	if err != nil {
		return err
	}
	if o.Tail != nil {
		return runTail(o, srcs)
	}
	since, err := ParseSince(o.Since, o.Now())
	if err != nil {
		return err
	}

	// Start following first and print the history after, so that a line written in between is in one
	// of them at least; a line in both is printed once (by its cursor).
	var live chan entry
	var closers []io.Closer
	if o.Follow {
		live = make(chan entry, 256)
		fctx, cancel := context.WithCancel(ctx)
		defer func() {
			cancel()
			for _, c := range closers {
				_ = c.Close()
			}
		}()
		var wg sync.WaitGroup
		for _, s := range srcs {
			rc, err := o.Journal.Start(fctx, journalArgs(o.Ref, s, 0, "", true))
			if err != nil {
				return err
			}
			closers = append(closers, rc)
			wg.Add(1)
			go func(s Source, rc io.Reader) {
				defer wg.Done()
				readEntries(rc, s, o.Ref, func(e entry) {
					select {
					case live <- e:
					case <-fctx.Done():
					}
				})
			}(s, rc)
		}
		go func() { wg.Wait(); close(live) }()
	}

	var all []entry
	for _, s := range srcs {
		es, err := history(ctx, o, s, since)
		if err != nil {
			return err
		}
		all = append(all, es...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ts.Before(all[j].ts) })
	if len(all) > o.Lines {
		all = all[len(all)-o.Lines:]
	}
	seen := make(map[string]bool, len(all))
	for _, e := range all {
		seen[e.cursor] = true
		printEntry(o.Out, e)
	}
	if len(all) == 0 && !o.Follow {
		noLines(o)
	}
	if !o.Follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-live:
			if !ok {
				// Every journalctl ended on its own: report the first failure, if any.
				for _, c := range closers {
					if err := c.Close(); err != nil {
						return err
					}
				}
				return nil
			}
			if e.cursor != "" && seen[e.cursor] {
				delete(seen, e.cursor)
				continue
			}
			printEntry(o.Out, e)
		}
	}
}

func noLines(o Options) {
	fmt.Fprintf(o.Err, "no log lines for %s", o.Ref)
	if o.Since != "" {
		fmt.Fprintf(o.Err, " since %s", o.Since)
	}
	fmt.Fprintln(o.Err, ". Check the ref with `supavise projects list`; a paused project writes nothing.")
	fmt.Fprintln(o.Err, "On a cluster, a node's journal holds the units that run on it: run the command on the server that hosts the project.")
	if !o.Root {
		fmt.Fprintln(o.Err, "This user may not read the system journal: run the command with sudo.")
	}
}

// journalArgs are the arguments of one journalctl for source s: the unit, JSON output with every
// field in full, at most lines entries (0 with follow starts after the end), the lines from since on,
// and, for a shared unit, only the lines that mention the ref.
//
// --all matters: without it, journalctl -o json prints a field of 4096 bytes or more as null (systemd's
// JSON_THRESHOLD in logs-show.c), so a long statement or JSON log line would show as an empty message,
// and a shared unit's long line would not match the ref.
func journalArgs(ref string, s Source, lines int, since string, follow bool) []string {
	args := []string{"-u", s.Unit, "--no-pager", "--all", "-o", "json", "-n", strconv.Itoa(lines)}
	if since != "" {
		args = append(args, "--since", since)
	}
	if s.Shared {
		args = append(args, "--grep", ref)
	}
	if follow {
		args = append(args, "-f")
	}
	return args
}

func history(ctx context.Context, o Options, s Source, since string) ([]entry, error) {
	if o.Lines == 0 {
		return nil, nil
	}
	rc, err := o.Journal.Start(ctx, journalArgs(o.Ref, s, o.Lines, since, false))
	if err != nil {
		return nil, err
	}
	var out []entry
	readEntries(rc, s, o.Ref, func(e entry) { out = append(out, e) })
	if err := rc.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// entry is one journal entry of a source.
type entry struct {
	ts     time.Time
	source string
	msg    string
	cursor string
}

// readEntries decodes journalctl's JSON output (one object per entry) and passes the entries of the
// project to fn. A shared unit's lines are kept only when they mention ref: journalctl's --grep
// already selected them, and this check also covers a journalctl that matches case-insensitively.
func readEntries(r io.Reader, s Source, ref string, fn func(entry)) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))
	for {
		var f map[string]json.RawMessage
		if err := dec.Decode(&f); err != nil {
			return // EOF, or a broken stream: Close reports a failing journalctl
		}
		e := entry{source: s.Name, msg: message(f["MESSAGE"])}
		if us, err := strconv.ParseInt(unquote(f["__REALTIME_TIMESTAMP"]), 10, 64); err == nil {
			e.ts = time.UnixMicro(us).UTC()
		}
		e.cursor = unquote(f["__CURSOR"])
		if s.Shared && !strings.Contains(e.msg, ref) {
			continue
		}
		fn(e)
	}
}

func unquote(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// message is the MESSAGE field: a string, or, when the line is not valid UTF-8, an array of bytes.
func message(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var ints []int
	if json.Unmarshal(raw, &ints) == nil {
		for _, v := range ints {
			b = append(b, byte(v))
		}
		return strings.ToValidUTF8(string(b), "�")
	}
	return ""
}

func printEntry(w io.Writer, e entry) {
	ts := "                        "
	if !e.ts.IsZero() {
		ts = e.ts.Format("2006-01-02T15:04:05.000Z")
	}
	fmt.Fprintf(w, "%s %-9s %s\n", ts, e.source, sanitize(strings.TrimRight(e.msg, "\r\n")))
}

// sanitize makes a log line safe to print on a terminal. Lines carry tenant-controlled text (a SQL
// statement in the Postgres log, a header in a request log), and a raw escape sequence in it would
// act on the operator's terminal.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0 {
			fmt.Fprintf(&b, "\\x%02x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var sinceRe = regexp.MustCompile(`^(\d+)([smhdw])$`)

// ParseSince turns the value of --since into the time journalctl takes: a relative age ("90s", "30m",
// "2h", "1d", "1w") counted back from now, or an absolute time ("2026-10-10T12:00:00Z",
// "2026-10-10 12:00:00" in UTC, or a date). Empty is no limit.
func ParseSince(v string, now time.Time) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	const layout = "2006-01-02 15:04:05"
	if m := sinceRe.FindStringSubmatch(v); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return "", fmt.Errorf("--since %q: %w", v, err)
		}
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.UTC().Add(-time.Duration(n)*unit).Format(layout) + " UTC", nil
	}
	for _, l := range []string{time.RFC3339, layout, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC().Format(layout) + " UTC", nil
		}
	}
	return "", fmt.Errorf("--since %q: use an age such as 30m, 2h or 1d, or a time such as 2026-10-10T12:00:00Z", v)
}

// runTail prints the end of each unit's log file. The exec backend keeps no timestamps, so the
// lines are shown source by source.
func runTail(o Options, srcs []Source) error {
	if o.Follow {
		fmt.Fprintln(o.Err, "--follow needs the systemd backend; showing the current end of the logs")
	}
	if o.Since != "" {
		return errors.New("--since needs the systemd backend: the exec backend's log files have no timestamps")
	}
	n := 0
	for _, s := range srcs {
		for _, line := range strings.Split(strings.TrimRight(o.Tail(s.Unit, o.Lines), "\n"), "\n") {
			if line == "" || s.Shared && !strings.Contains(line, o.Ref) {
				continue
			}
			fmt.Fprintf(o.Out, "%-9s %s\n", s.Name, sanitize(line))
			n++
		}
	}
	if n == 0 {
		noLines(o)
	}
	return nil
}
