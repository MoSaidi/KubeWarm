package controller

import (
	"kubewarm/internal/kube"
	"kubewarm/internal/model"
	"sort"
)

// SetNodes replaces authoritative metadata. Failed API refreshes clear this
// view, failing targeted warming closed until a fresh list succeeds.
func (r *Registry) SetNodes(nodes []kube.Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = make(map[string]kube.Node, len(nodes))
	for _, n := range nodes {
		r.nodes[n.Metadata.Name] = n
	}
}
func (r *Registry) Nodes() map[string]kube.Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]kube.Node, len(r.nodes))
	for k, v := range r.nodes {
		out[k] = v
	}
	return out
}
func (r *Registry) preferZone(target string, sources []model.RescueSource) {
	nodes := r.Nodes()
	zone := nodes[target].Metadata.Labels["topology.kubernetes.io/zone"]
	if zone == "" {
		return
	}
	sort.SliceStable(sources, func(i, j int) bool {
		return nodes[sources[i].NodeID].Metadata.Labels["topology.kubernetes.io/zone"] == zone && nodes[sources[j].NodeID].Metadata.Labels["topology.kubernetes.io/zone"] != zone
	})
}
func (r *Registry) pickZoneSource(holders []string, target string, serving map[string]int, cap int) string {
	nodes := r.Nodes()
	zone := nodes[target].Metadata.Labels["topology.kubernetes.io/zone"]
	var local []string
	if zone != "" {
		for _, h := range holders {
			if nodes[h].Metadata.Labels["topology.kubernetes.io/zone"] == zone {
				local = append(local, h)
			}
		}
	}
	if h := pickSource(local, target, serving, cap); h != "" {
		return h
	}
	return pickSource(holders, target, serving, cap)
}
