package app

import (
	"context"
	"errors"
	"time"
)

var errVPNMaintenanceSkipped = errors.New("VPN maintenance skipped because exit state changed")

// Probe/rotation is bounded and only touches idle managed WireGuard exits.
// Other exits continue serving while one container drains and changes servers.
func (e *Egress) maintainVPNs(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.maintainVPNPass(ctx)
		}
	}
}
func (e *Egress) maintainVPNPass(ctx context.Context) {
	e.mu.Lock()
	endpoints := append([]EgressEndpoint{}, e.config.Endpoints...)
	activeMode := e.config.Mode != "direct"
	e.mu.Unlock()
	if !activeMode {
		return
	}
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return
		}
		if !endpoint.Enabled || endpoint.Kind != "vpn" {
			continue
		}
		e.mu.Lock()
		r := e.routes[endpoint.ID]
		if r == nil || r.Active > 0 || r.Rotating {
			e.mu.Unlock()
			continue
		}
		delay := time.Duration(min(10, 1<<min(r.MaintenanceFailures, 4))) * 30 * time.Second
		if time.Since(r.LastMaintenance) < delay {
			e.mu.Unlock()
			continue
		}
		rotate := !r.Healthy || e.limits[routeKey(r)][""].Until > time.Now().Unix()
		if r.IP != "" {
			for id, other := range e.routes {
				if id < r.Endpoint.ID && other.Endpoint.Kind == "vpn" && other.Endpoint.Enabled && other.Healthy && other.IP == r.IP {
					rotate = true
				}
			}
		}
		needsProbe := time.Since(r.LastProbe) > 60*time.Second
		if !rotate && !needsProbe {
			e.mu.Unlock()
			continue
		}
		r.LastMaintenance = time.Now()
		e.mu.Unlock()
		var err error
		changed := true
		if rotate && endpoint.RotateOnLimit {
			op, cancel := context.WithTimeout(ctx, 60*time.Second)
			body, rotateErr := e.Rotate(op, endpoint.ID, RotationOptions{MaxAttempts: 3, idleOnly: true, expected: endpoint})
			cancel()
			err = rotateErr
			changed, _ = body["changed"].(bool)
		} else {
			op, cancel := context.WithTimeout(ctx, 15*time.Second)
			body, probeErr := e.Probe(op, endpoint.ID)
			err = probeErr
			if healthy, _ := body["healthy"].(bool); err == nil && !healthy {
				err = errors.New("VPN is unhealthy")
			}
			cancel()
		}
		if errors.Is(err, errVPNMaintenanceSkipped) {
			continue
		}
		e.mu.Lock()
		if r = e.routes[endpoint.ID]; r != nil {
			if err != nil || !changed {
				r.MaintenanceFailures++
			} else {
				r.MaintenanceFailures = 0
			}
		}
		e.mu.Unlock()
	}
}
