/*
Copyright 2026 The Understudy Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	SurgesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "understudy_surges_total",
		Help: "Surges initiated, labeled by the doom signal class that triggered them.",
	}, []string{"class"})

	RelaxedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "understudy_hold_relaxed_total",
		Help: "Eviction holds relaxed without a ready understudy, by reason.",
	}, []string{"reason"})

	EvictionAttemptsObserved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "understudy_eviction_attempts_observed_total",
		Help: "Eviction attempts seen by the webhook, by whether the pod is protected by an Understudy.",
	}, []string{"protected"})

	EvictionsHeld = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "understudy_evictions_held_total",
		Help: "Evictions of protected pods answered with 429 at admission because no stand-in was ready yet.",
	})

	blockedMu    sync.Mutex
	blockedSince = map[string]time.Time{}

	OldestBlockedEvictionSeconds = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "understudy_oldest_blocked_eviction_seconds",
		Help: "Age in seconds of the oldest currently-blocked eviction across all Understudy targets (0 when nothing is blocked). Alert on this.",
	}, func() float64 {
		blockedMu.Lock()
		defer blockedMu.Unlock()
		var oldest float64
		now := time.Now()
		for _, t := range blockedSince {
			if age := now.Sub(t).Seconds(); age > oldest {
				oldest = age
			}
		}
		return oldest
	})
)

func TrackBlocked(key string, since time.Time) {
	blockedMu.Lock()
	defer blockedMu.Unlock()
	blockedSince[key] = since
}

func UntrackBlocked(key string) {
	blockedMu.Lock()
	defer blockedMu.Unlock()
	delete(blockedSince, key)
}

func init() {
	ctrlmetrics.Registry.MustRegister(SurgesTotal, RelaxedTotal, EvictionAttemptsObserved, EvictionsHeld, OldestBlockedEvictionSeconds)
}
