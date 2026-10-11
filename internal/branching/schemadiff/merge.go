package schemadiff

import (
	"reflect"
	"sort"
)

// Same reports whether two objects define the same thing. The keys an object needs (Deps) are not
// compared: they follow from what else exists.
func (o *Object) Same(p *Object) bool {
	if o == nil || p == nil {
		return o == p
	}
	return o.Kind == p.Kind && o.Key == p.Key && o.Name == p.Name && o.Parent == p.Parent && o.Def == p.Def &&
		reflect.DeepEqual(nilIfEmpty(o.Attr), nilIfEmpty(p.Attr)) && equalStrings(o.List, p.List)
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// Merge3 combines two descendants of base object by object: it returns ours with what theirs
// changed since base (an object added, changed or removed) wherever ours did not change it too.
// An object both changed in different ways is a conflict; the merged snapshot keeps ours for it, or
// theirs when preferTheirs is set. A nil base is an empty one.
//
// The plan from ours to the merged snapshot (Diff) is what it takes to bring theirs' changes into
// ours: a push takes the parent's schema changes into a branch this way.
func Merge3(base, ours, theirs *Snapshot, preferTheirs bool) (merged *Snapshot, conflicts []string) {
	if base == nil {
		base = &Snapshot{Version: FormatVersion}
	}
	bi, oi, ti := base.index(), ours.index(), theirs.index()
	keys := map[string]struct{}{}
	for k := range bi {
		keys[k] = struct{}{}
	}
	for k := range oi {
		keys[k] = struct{}{}
	}
	for k := range ti {
		keys[k] = struct{}{}
	}
	merged = &Snapshot{Version: FormatVersion, PG: ours.PG, Schemas: ours.Schemas, Unmodeled: ours.Unmodeled}
	for k := range keys {
		b, o, t := bi[k], oi[k], ti[k]
		pick := o
		switch {
		case o.Same(t):
		case o.Same(b):
			pick = t
		case t.Same(b):
		default:
			conflicts = append(conflicts, describeKey(k, o, t))
			if preferTheirs {
				pick = t
			}
		}
		if pick != nil {
			merged.Objects = append(merged.Objects, *pick)
		}
	}
	merged.Sort()
	sort.Strings(conflicts)
	return merged, conflicts
}

// Changed lists the keys of the objects that differ between two snapshots (added, removed or
// changed), sorted. It is how a caller tells whether a side moved at all.
func Changed(a, b *Snapshot) []string {
	if a == nil {
		a = &Snapshot{}
	}
	if b == nil {
		b = &Snapshot{}
	}
	ai, bi := a.index(), b.index()
	var out []string
	for k, x := range ai {
		if !x.Same(bi[k]) {
			out = append(out, k)
		}
	}
	for k := range bi {
		if _, ok := ai[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func describeKey(k string, o, t *Object) string {
	what := "changed in both"
	switch {
	case o == nil:
		what = "removed on one side and changed on the other"
	case t == nil:
		what = "changed on one side and removed on the other"
	}
	return friendlyKey(k) + " (" + what + ")"
}

// friendlyKey turns an object key into "kind schema.name".
func friendlyKey(k string) string {
	kind, rest := k, ""
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			kind, rest = k[:i], k[i+1:]
			break
		}
	}
	out := kind + " "
	for i := 0; i < len(rest); i++ {
		if rest[i] == '|' {
			out += "."
		} else {
			out += string(rest[i])
		}
	}
	return out
}

// Restrict returns the snapshot without the objects of schemas outside the list: the view of
// a snapshot taken for more schemas as if it had been taken for these.
func (s *Snapshot) Restrict(schemas []string) *Snapshot {
	want := map[string]bool{}
	for _, n := range schemas {
		want[n] = true
	}
	out := &Snapshot{Version: s.Version, PG: s.PG, Unmodeled: s.Unmodeled}
	for _, n := range s.Schemas {
		if want[n] {
			out.Schemas = append(out.Schemas, n)
		}
	}
	for _, o := range s.Objects {
		schema, _ := splitKey(o.Key)
		switch o.Kind {
		case KindACL, KindComment:
			schema, _ = splitKey(o.Parent)
		case KindDefACL:
			if o.Attr["schema"] == "" {
				out.Objects = append(out.Objects, o)
				continue
			}
			schema = o.Attr["schema"]
		}
		if o.Kind == KindExtension || want[schema] {
			out.Objects = append(out.Objects, o)
		}
	}
	return out
}

// Covers reports whether the snapshot was taken for all of these schemas.
func (s *Snapshot) Covers(schemas []string) bool {
	have := map[string]bool{}
	for _, n := range s.Schemas {
		have[n] = true
	}
	for _, n := range schemas {
		if !have[n] {
			return false
		}
	}
	return true
}

// Sync returns base advanced to where a and b agree: for each object both have in the same form
// (or both lack), the new base holds that form; where they differ it keeps base's. It is how the
// common ancestor of a branch and its parent moves after a push or a merge: the changes that crossed
// become part of the base, and what is still different stays a change of one side.
func Sync(base, a, b *Snapshot) *Snapshot {
	if base == nil {
		base = &Snapshot{Version: FormatVersion}
	}
	bi, ai, ci := base.index(), a.index(), b.index()
	keys := map[string]struct{}{}
	for k := range bi {
		keys[k] = struct{}{}
	}
	for k := range ai {
		keys[k] = struct{}{}
	}
	for k := range ci {
		keys[k] = struct{}{}
	}
	out := &Snapshot{Version: FormatVersion, PG: a.PG, Schemas: a.Schemas, Unmodeled: base.Unmodeled}
	if len(base.Schemas) > 0 {
		out.Schemas = base.Schemas
	}
	for k := range keys {
		x, y := ai[k], ci[k]
		pick := bi[k]
		if x.Same(y) {
			pick = x
		}
		if pick != nil {
			out.Objects = append(out.Objects, *pick)
		}
	}
	out.Sort()
	return out
}
