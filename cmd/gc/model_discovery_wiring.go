package main

import (
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

// installProviderModelDiscovery gives this gc process its model discoverer:
// provider resolution and the dashboard's provider picker then widen each
// provider's curated model list with the ids its binary reports, cached per
// binary version (ga-k67).
//
// It is wired at the process entry funnel rather than in the command tree so
// that in-process tests — which build the root command directly — keep the
// default nil discoverer and never exec a provider binary.
func installProviderModelDiscovery() {
	config.SetModelDiscoverer(api.DefaultProviderModelDiscovery())
}
