package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kubewarm/internal/kube"
	"kubewarm/internal/model"
)

var testDigest = "docker.io/library/nginx@sha256:" + strings.Repeat("a", 64)

func warmNode(name, zone, group string) kube.Node {
	var n kube.Node
	raw := `{"metadata":{"name":"` + name + `","labels":{"workload":"` + group + `","topology.kubernetes.io/zone":"` + zone + `"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
	_ = json.Unmarshal([]byte(raw), &n)
	return n
}
func warmReq() WarmRequest {
	return WarmRequest{Image: testDigest, NodeSelector: map[string]string{"workload": "api"}, MinReady: 2, Zones: []string{"a", "b"}, MinReadyPerZone: 1, TimeoutSeconds: 60}
}
func TestWarmValidation(t *testing.T) {
	r := warmReq()
	if err := validateWarm(&r); err != nil {
		t.Fatal(err)
	}
	cases := []WarmRequest{warmReq(), warmReq(), warmReq(), warmReq(), warmReq()}
	cases[0].Image = "nginx:latest"
	cases[1].NodeSelector = nil
	cases[2].MinReady = 0
	cases[3].TimeoutSeconds = 3601
	cases[4].Zones = []string{"a", "a"}
	for i, r := range cases {
		if validateWarm(&r) == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func TestZoneSourceCapacityFallback(t *testing.T) {
	r := NewRegistry(time.Minute, time.Minute)
	r.SetNodes([]kube.Node{warmNode("target", "a", "api"), warmNode("near", "a", "api"), warmNode("far", "b", "api")})
	if got := r.pickZoneSource([]string{"far", "near"}, "target", map[string]int{}, 1); got != "near" {
		t.Fatal(got)
	}
	if got := r.pickZoneSource([]string{"far", "near"}, "target", map[string]int{"near": 1}, 1); got != "far" {
		t.Fatal(got)
	}
}
func TestWarmReadinessZoneAndFreshness(t *testing.T) {
	r := NewRegistry(time.Minute, time.Minute)
	r.SetNodes([]kube.Node{warmNode("one", "a", "api"), warmNode("two", "b", "api"), warmNode("gpu", "b", "gpu")})
	now := time.Now()
	r.Update(model.WorkerReport{NodeID: "one", Timestamp: now, Images: []string{testDigest}, Utilization: .9})
	r.Update(model.WorkerReport{NodeID: "two", Timestamp: now.Add(-2 * time.Minute), Images: []string{testDigest}, Utilization: .9})
	r.Update(model.WorkerReport{NodeID: "gpu", Timestamp: now, Images: []string{testDigest}, Utilization: .9})
	w := NewWarmer(r, nil, "token", "")
	j := &WarmJob{ID: "j", Request: warmReq(), State: "Queued", CreatedAt: now, Errors: map[string]string{}}
	w.jobs[j.ID] = j
	w.reconcile(context.Background(), now)
	if j.State != "Running" || len(j.Ready) != 1 {
		t.Fatalf("false readiness: %+v", j)
	}
	r.Update(model.WorkerReport{NodeID: "two", Timestamp: now, Images: []string{testDigest}, Utilization: .9})
	w.reconcile(context.Background(), now)
	if j.State != "Ready" {
		t.Fatalf("not ready: %+v", j)
	}
}
func TestWarmTimeoutAndAPIAuth(t *testing.T) {
	r := NewRegistry(time.Minute, time.Minute)
	w := NewWarmer(r, nil, "shared", "")
	s := NewServer(r, nil)
	s.SetWebhookToken(strings.Repeat("t", 32))
	w.Register(s)
	body, _ := json.Marshal(warmReq())
	req := httptest.NewRequest("POST", "/v1/warmings", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	req = httptest.NewRequest("POST", "/v1/warmings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 202 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var j WarmJob
	_ = json.Unmarshal(rr.Body.Bytes(), &j)
	w.reconcile(context.Background(), j.CreatedAt.Add(61*time.Second))
	if w.jobs[j.ID].State != "TimedOut" {
		t.Fatal(w.jobs[j.ID].State)
	}
}
func TestPolicyNewNodeAndPullAcceptanceIsNotReady(t *testing.T) {
	var calls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer shared" {
			t.Error("missing worker auth")
		}
		calls.Add(1)
		writeJSON(rw, 202, model.PullAck{Accepted: true})
	}))
	defer worker.Close()
	r := NewRegistry(time.Minute, time.Minute)
	w := NewWarmer(r, nil, "shared", "")
	w.policies = []WarmPolicy{{Name: "api", Image: testDigest, NodeSelector: map[string]string{"workload": "api"}}}
	w.reconcile(context.Background(), time.Now())
	if calls.Load() != 0 {
		t.Fatal("ordered nonexistent node")
	}
	r.SetNodes([]kube.Node{warmNode("new", "a", "api"), warmNode("gpu", "a", "gpu")})
	for _, n := range []string{"new", "gpu"} {
		r.Update(model.WorkerReport{NodeID: n, Address: strings.TrimPrefix(worker.URL, "http://"), Timestamp: time.Now()})
	}
	req := warmReq()
	req.MinReady = 1
	req.Zones = nil
	req.MinReadyPerZone = 0
	j := &WarmJob{ID: "j", Request: req, CreatedAt: time.Now(), State: "Queued", Errors: map[string]string{}}
	w.jobs[j.ID] = j
	w.reconcile(context.Background(), time.Now())
	w.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected only matching node pull, got %d", calls.Load())
	}
	w.reconcile(context.Background(), time.Now())
	w.wg.Wait()
	if j.State == "Ready" {
		t.Fatal("acceptance must not count as ready")
	}
	r.Update(model.WorkerReport{NodeID: "new", Address: strings.TrimPrefix(worker.URL, "http://"), Timestamp: time.Now(), Images: []string{testDigest}})
	w.reconcile(context.Background(), time.Now())
	if j.State != "Ready" {
		t.Fatal(j.State)
	}
}
func TestPeerFailureFallsBackToRegistry(t *testing.T) {
	var paths []string
	worker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/rescue" {
			writeJSON(rw, 500, model.RescueResult{Error: "peer unavailable"})
			return
		}
		writeJSON(rw, 202, model.PullAck{Accepted: true})
	}))
	defer worker.Close()
	w := NewWarmer(NewRegistry(time.Minute, time.Minute), nil, "shared", "")
	err := w.transfer(context.Background(), &workerEntry{Address: strings.TrimPrefix(worker.URL, "http://")}, testDigest, []model.RescueSource{{NodeID: "peer", Address: "peer:18081"}})
	if err != nil || strings.Join(paths, ",") != "/rescue,/pull" {
		t.Fatalf("%v %v", err, paths)
	}
}
func TestInvalidPoliciesFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.json")
	_ = os.WriteFile(path, []byte(`[{"name":"bad","image":"nginx:latest","nodeSelector":{"workload":"api"}}]`), 0600)
	w := NewWarmer(NewRegistry(time.Minute, time.Minute), nil, "shared", path)
	w.policies = []WarmPolicy{{Name: "old"}}
	w.reloadPolicies()
	if len(w.policies) != 0 {
		t.Fatal("invalid policies retained")
	}
}
