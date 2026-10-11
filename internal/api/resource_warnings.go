package api

import (
	"errors"
	"net/http"

	plat "github.com/supavise/supavise/internal/api/gen/platform"
	"github.com/supavise/supavise/internal/metrics"
	"github.com/supavise/supavise/internal/registry"
)

// GET /platform/projects-resource-warnings: the banners Studio shows above a project when it is
// running out of something. The collector answers for the three it measures, the disk against the
// project's quota (80 percent a warning, 90 percent critical), the CPU against its quota and the
// memory against its limit; the others are null. Supavise has no read-only mode for a full disk, so
// is_readonly_mode_enabled is always false: a project past its quota fails its writes with ENOSPC.
//
// The query names a project (ref) or an organization (slug); with neither, the answer is for every
// project the caller can see. Studio asks with ref and picks its own project out of the list.

func (s *Server) projectsResourceWarnings(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	ps, err := s.userProjects(ctx)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	var picked []registry.Project
	switch ref, slug := q.Get("ref"), q.Get("slug"); {
	case ref != "":
		// A branch is a project of its own with its own warnings, visible to whoever sees its parent.
		p, err := s.loadProject(ctx, ref)
		if err != nil {
			if errors.Is(err, errNoProject) {
				writeJSON(w, http.StatusOK, []any{})
				return nil
			}
			return err
		}
		root := p.Ref
		if p.Branch != nil {
			root = p.Branch.ParentRef
		}
		for _, v := range ps {
			if v.Ref == root {
				picked = append(picked, *p)
			}
		}
	case slug != "":
		for _, v := range ps {
			if org, err := s.orgOf(ctx, &v); err == nil && org.Slug == slug {
				picked = append(picked, v)
			}
		}
	default:
		picked = ps
	}
	out := make([]plat.ProjectResourceWarningsResponseOutput, 0, len(picked))
	for _, p := range picked {
		out = append(out, s.resourceWarnings(p.Ref))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) resourceWarnings(ref string) plat.ProjectResourceWarningsResponseOutput {
	var w metrics.Warnings
	if s.metrics != nil {
		w, _ = s.metrics.Warnings(ref)
	}
	out := plat.ProjectResourceWarningsResponseOutput{Project: ref}
	if l := warningLevel(w.DiskSpace); l != "" {
		v := plat.ProjectResourceWarningsResponseOutputDiskSpaceExhaustion(l)
		out.DiskSpaceExhaustion = &v
	}
	if l := warningLevel(w.CPU); l != "" {
		v := plat.ProjectResourceWarningsResponseOutputCpuExhaustion(l)
		out.CpuExhaustion = &v
	}
	if l := warningLevel(w.Memory); l != "" {
		v := plat.ProjectResourceWarningsResponseOutputMemoryAndSwapExhaustion(l)
		out.MemoryAndSwapExhaustion = &v
	}
	return out
}

func warningLevel(l metrics.Level) string { return string(l) }
