package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"kubewarm/internal/imageref"
	"kubewarm/internal/kube"
	"kubewarm/internal/logging"
	"kubewarm/internal/model"
	"kubewarm/internal/sharedtoken"
)

type WarmRequest struct {
	Image           string            `json:"image"`
	NodeSelector    map[string]string `json:"nodeSelector"`
	MinReady        int               `json:"minReady"`
	Zones           []string          `json:"zones,omitempty"`
	MinReadyPerZone int               `json:"minReadyPerZone,omitempty"`
	TimeoutSeconds  int               `json:"timeoutSeconds"`
}
type WarmJob struct {
	ID           string            `json:"id"`
	Request      WarmRequest       `json:"request"`
	State        string            `json:"state"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	Ready        []string          `json:"ready"`
	Eligible     int               `json:"eligible"`
	ReadyPerZone map[string]int    `json:"readyPerZone"`
	Message      string            `json:"message,omitempty"`
	Errors       map[string]string `json:"errors,omitempty"`
}
type WarmPolicy struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	NodeSelector map[string]string `json:"nodeSelector"`
}
type NodeLister interface {
	ListNodes(context.Context) ([]kube.Node, error)
}

// Warmer is intentionally single-controller. CI jobs are bounded and in memory;
// policies are reloaded from the projected ConfigMap and reconcile continuously.
type Warmer struct {
	mu         sync.Mutex
	registry   *Registry
	api        NodeLister
	token      string
	policyPath string
	jobs       map[string]*WarmJob
	policies   []WarmPolicy
	busy       map[string]bool
	next       map[string]time.Time
	client     *http.Client
	sem        chan struct{}
	wg         sync.WaitGroup
}

func NewWarmer(r *Registry, api NodeLister, token, policyPath string) *Warmer {
	return &Warmer{registry: r, api: api, token: token, policyPath: policyPath, jobs: map[string]*WarmJob{}, busy: map[string]bool{}, next: map[string]time.Time{}, client: &http.Client{Timeout: 2 * time.Minute}, sem: make(chan struct{}, 4)}
}
func validateWarm(r *WarmRequest) error {
	normalized := imageref.Normalize(strings.TrimSpace(r.Image))
	parsed, err := reference.ParseNormalizedNamed(normalized)
	if err != nil {
		return fmt.Errorf("invalid image reference")
	}
	if _, ok := parsed.(reference.Digested); !ok {
		return fmt.Errorf("image must use an immutable repo@sha256:digest reference")
	}
	if !strings.Contains(normalized, "@sha256:") {
		return fmt.Errorf("sha256 image digest required")
	}
	r.Image = normalized
	if len(r.NodeSelector) == 0 {
		return fmt.Errorf("nodeSelector must explicitly select at least one label")
	}
	for k, v := range r.NodeSelector {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("selector keys and values must be nonempty")
		}
	}
	if r.MinReady < 1 || r.MinReady > 10000 {
		return fmt.Errorf("minReady must be 1..10000")
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 3600 {
		return fmt.Errorf("timeoutSeconds must be 1..3600")
	}
	if len(r.Zones) > 0 && r.MinReadyPerZone < 1 {
		return fmt.Errorf("minReadyPerZone must be positive when zones are specified")
	}
	if r.MinReadyPerZone > 10000 || (len(r.Zones) == 0 && r.MinReadyPerZone != 0) {
		return fmt.Errorf("invalid per-zone threshold")
	}
	seen := map[string]bool{}
	for _, z := range r.Zones {
		if z == "" || seen[z] {
			return fmt.Errorf("zones must be nonempty and unique")
		}
		seen[z] = true
	}
	return nil
}
func matches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
func eligible(n kube.Node, r WarmRequest) bool {
	if !n.Ready() || !matches(n.Metadata.Labels, r.NodeSelector) {
		return false
	}
	if len(r.Zones) > 0 && !contains(r.Zones, n.Metadata.Labels["topology.kubernetes.io/zone"]) {
		return false
	}
	return true
}
func (w *Warmer) Register(s *Server) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if !sharedtoken.Valid(r, s.webhook.Token) {
				http.Error(rw, "unauthorized", 401)
				return
			}
			h(rw, r)
		}
	}
	s.mux.HandleFunc("/v1/warmings", guard(w.handleCreate))
	s.mux.HandleFunc("/v1/warmings/", guard(w.handleGet))
}
func (w *Warmer) handleCreate(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(rw, "method not allowed", 405)
		return
	}
	var request WarmRequest
	d := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&request); err != nil {
		http.Error(rw, "invalid request JSON", 400)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(rw, "one JSON object required", 400)
		return
	}
	if err := validateWarm(&request); err != nil {
		http.Error(rw, err.Error(), 400)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.jobs) >= 1000 {
		http.Error(rw, "job capacity reached; retry later", 503)
		return
	}
	now := time.Now()
	j := &WarmJob{ID: newJobID(), Request: request, State: "Queued", CreatedAt: now, UpdatedAt: now, Ready: []string{}, ReadyPerZone: map[string]int{}, Errors: map[string]string{}}
	w.jobs[j.ID] = j
	rw.Header().Set("Location", "/v1/warmings/"+j.ID)
	writeJSON(rw, 202, j)
}
func (w *Warmer) handleGet(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(rw, "method not allowed", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/warmings/")
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[id]
	if !ok {
		http.Error(rw, "job not found (jobs do not survive controller restart)", 404)
		return
	}
	writeJSON(rw, 200, j)
}
func (w *Warmer) reloadPolicies() {
	if w.policyPath == "" {
		return
	}
	data, err := os.ReadFile(w.policyPath)
	var policies []WarmPolicy
	if err == nil {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		err = d.Decode(&policies)
		if err == nil {
			var extra any
			if d.Decode(&extra) != io.EOF {
				err = fmt.Errorf("trailing policy JSON")
			}
		}
	}
	if err == nil && len(policies) > 100 {
		err = fmt.Errorf("at most 100 policies allowed")
	}
	seen := map[string]bool{}
	if err == nil {
		for i := range policies {
			p := &policies[i]
			r := WarmRequest{Image: p.Image, NodeSelector: p.NodeSelector, MinReady: 1, TimeoutSeconds: 300}
			if p.Name == "" || seen[p.Name] {
				err = fmt.Errorf("policy names must be nonempty and unique")
				break
			}
			seen[p.Name] = true
			if err = validateWarm(&r); err != nil {
				break
			}
			p.Image = r.Image
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.policies = nil
		logging.Warnf("kubewarm: policy reload failed; policy warming paused: %v", err)
		return
	}
	w.policies = policies
}
func (w *Warmer) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer w.wg.Wait()
	for {
		w.reloadPolicies()
		nodes, err := w.api.ListNodes(ctx)
		if err != nil {
			nodes = nil
			logging.Warnf("kubewarm: node refresh failed; targeted warming paused: %v", err)
		}
		w.registry.SetNodes(nodes)
		w.reconcile(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (w *Warmer) reconcile(ctx context.Context, now time.Time) {
	nodes := w.registry.Nodes()
	fresh := w.registry.FreshWorkers()
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, t := range w.next {
		if now.After(t.Add(time.Hour)) {
			delete(w.next, key)
		}
	}
	for id, j := range w.jobs {
		if terminal(j.State) {
			if now.Sub(j.UpdatedAt) > time.Hour {
				delete(w.jobs, id)
			}
			continue
		}
		j.UpdatedAt = now
		if !now.Before(j.CreatedAt.Add(time.Duration(j.Request.TimeoutSeconds) * time.Second)) {
			j.State = "TimedOut"
			j.Message = "readiness threshold was not reached before deadline"
			continue
		}
		j.State = "Running"
		j.Ready = []string{}
		j.ReadyPerZone = map[string]int{}
		j.Eligible = 0
		for _, f := range fresh {
			if !eligible(nodes[f.NodeID], j.Request) {
				continue
			}
			j.Eligible++
			if f.HasImage(j.Request.Image) {
				j.Ready = append(j.Ready, f.NodeID)
				j.ReadyPerZone[nodes[f.NodeID].Metadata.Labels["topology.kubernetes.io/zone"]]++
				delete(j.Errors, f.NodeID)
			}
		}
		sort.Strings(j.Ready)
		ready := len(j.Ready) >= j.Request.MinReady
		for _, z := range j.Request.Zones {
			if j.ReadyPerZone[z] < j.Request.MinReadyPerZone {
				ready = false
			}
		}
		if ready {
			j.State = "Ready"
			j.Message = "threshold observed in fresh worker inventories"
			continue
		}
		j.Message = fmt.Sprintf("%d ready of %d eligible fresh workers; waiting for required total and zone coverage", len(j.Ready), j.Eligible)
		w.schedule(ctx, j.Request, fresh, nodes, j, now)
	}
	for _, p := range w.policies {
		w.schedule(ctx, WarmRequest{Image: p.Image, NodeSelector: p.NodeSelector}, fresh, nodes, nil, now)
	}
}
func terminal(s string) bool { return s == "Ready" || s == "TimedOut" || s == "Failed" }
func (w *Warmer) schedule(ctx context.Context, r WarmRequest, fresh []*workerEntry, nodes map[string]kube.Node, j *WarmJob, now time.Time) {
	for _, f := range fresh {
		key := f.NodeID + "\n" + r.Image
		if !eligible(nodes[f.NodeID], r) || f.HasImage(r.Image) || f.Utilization >= 0.70 || w.busy[f.NodeID] || now.Before(w.next[key]) {
			continue
		}
		select {
		case w.sem <- struct{}{}:
		default:
			return
		}
		var sources []model.RescueSource
		for _, h := range fresh {
			if h.NodeID != f.NodeID && h.HasImage(r.Image) {
				sources = append(sources, model.RescueSource{NodeID: h.NodeID, Address: h.Address})
			}
		}
		w.registry.preferZone(f.NodeID, sources)
		if len(sources) > 3 {
			sources = sources[:3]
		}
		w.busy[f.NodeID] = true
		w.next[key] = now.Add(time.Minute)
		w.wg.Add(1)
		go func(target *workerEntry, sources []model.RescueSource, key string, j *WarmJob) {
			defer w.wg.Done()
			defer func() { <-w.sem }()
			err := w.transfer(ctx, target, r.Image, sources)
			w.mu.Lock()
			defer w.mu.Unlock()
			delete(w.busy, target.NodeID)
			w.next[key] = time.Now().Add(time.Minute)
			if j != nil && !terminal(j.State) {
				if err != nil {
					j.Errors[target.NodeID] = err.Error()
				} else {
					delete(j.Errors, target.NodeID)
				}
			}
		}(f, sources, key, j)
	}
}
func (w *Warmer) transfer(ctx context.Context, target *workerEntry, image string, sources []model.RescueSource) error {
	if len(sources) > 0 {
		_, err := sendRescueOrder(ctx, w.client, w.token, target.Address, model.RescueOrder{Image: image, Sources: sources, Reason: "propagate"})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	body, _ := json.Marshal(model.PullOrder{Image: image, OrderedAt: time.Now()})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+target.Address+"/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	sharedtoken.Set(req, w.token)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var ack model.PullAck
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ack); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !ack.Accepted {
		return fmt.Errorf("worker pull not accepted: status %d: %s", resp.StatusCode, ack.Reason)
	}
	return nil // acceptance is NOT readiness: only fresh inventory can satisfy a job
}
