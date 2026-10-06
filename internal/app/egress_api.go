package app

import (
	"context"
	"net/http"
	"sync"
	"time"
)

func (a *App) getEgress(w http.ResponseWriter, r *http.Request) {
	// Pooled health is a cheap snapshot of live device leases, including
	// streaming flows. Poll it without altering tunnels or cached health on
	// transient management errors.
	a.egress.mu.Lock()
	ids := []string{}
	for id, route := range a.egress.routes {
		if route.Pooled && route.Endpoint.Enabled {
			ids = append(ids, id)
		}
	}
	a.egress.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	for _, id := range ids {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			_, _ = a.egress.Probe(ctx, id, true)
		}(id)
	}
	wait.Wait()
	writeJSON(w, 200, a.egress.Snapshot())
}
func (a *App) putEgress(w http.ResponseWriter, r *http.Request) {
	var config EgressConfig
	if err := readJSON(w, r, &config); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err := a.egress.Save(config); err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.pool.mu.Lock()
	ids := []string{}
	for id := range a.pool.workers {
		ids = append(ids, id)
	}
	a.pool.mu.Unlock()
	for _, id := range ids {
		a.pool.drain(id)
	}
	writeJSON(w, 200, a.egress.Snapshot())
}
func (a *App) probeEgress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := a.egress.Probe(ctx, r.PathValue("id"))
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
func (a *App) rotateEgress(w http.ResponseWriter, r *http.Request) {
	var options RotationOptions
	if r.ContentLength != 0 {
		if err := readJSON(w, r, &options); err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := a.egress.Rotate(ctx, r.PathValue("id"), options)
	if err != nil {
		problem(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
