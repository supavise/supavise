package proxy

import (
	"time"

	"github.com/supavise/supavise/internal/reqstats"
)

// statsService is the group the requests of route rt are counted under in Options.Stats: one per
// path prefix of the project's API, as the dashboard's request charts group them.
func (rt *route) statsService() (reqstats.Service, bool) {
	switch rt.svc {
	case svcRest:
		if rt.prefix == "/graphql/v1" {
			return reqstats.GraphQL, true
		}
		return reqstats.Rest, true
	case svcAuth:
		return reqstats.Auth, true
	case svcStorage:
		return reqstats.Storage, true
	case svcRealtime:
		return reqstats.Realtime, true
	case svcFunctions:
		return reqstats.Functions, true
	}
	return 0, false
}

// countRequest adds a request that matched route rt to project ref's counts. It runs for every
// project request, whatever the answer turns out to be (a refused key is a request too), and
// before the request is served, so that a WebSocket that stays open for hours is counted when it
// starts. A CORS preflight and a path that matches no route are not counted. Nil Options.Stats
// counts nothing.
func (s *Server) countRequest(ref string, rt *route, at time.Time) {
	if s.opts.Stats == nil {
		return
	}
	if svc, ok := rt.statsService(); ok {
		s.opts.Stats.Add(ref, svc, at)
	}
}
