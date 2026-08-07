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

package signal

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type Class string

const (
	VoluntaryUnbounded Class = "VoluntaryUnbounded"
	Involuntary        Class = "Involuntary"
)

type NodeDoom struct {
	Node      string
	Class     Class
	Deadline  *time.Time
	ExpiresAt *time.Time
	Source    string
}

func (d NodeDoom) expired(now time.Time) bool {
	return d.ExpiresAt != nil && now.After(*d.ExpiresAt)
}

type Adapter interface {
	Name() string
	SetupWithManager(mgr ctrl.Manager, reg *Registry) error
}

type Registry struct {
	mu    sync.RWMutex
	dooms map[string]map[string]NodeDoom
	subs  []chan event.GenericEvent
}

func NewRegistry() *Registry {
	return &Registry{dooms: map[string]map[string]NodeDoom{}}
}

func (r *Registry) Set(d NodeDoom) {
	r.mu.Lock()
	bySource, ok := r.dooms[d.Node]
	if !ok {
		bySource = map[string]NodeDoom{}
		r.dooms[d.Node] = bySource
	}
	prev, existed := bySource[d.Source]
	bySource[d.Source] = d
	r.mu.Unlock()
	if !existed || !equalDoom(prev, d) {
		r.notify(d.Node)
	}
}

func (r *Registry) Clear(node, source string) {
	r.mu.Lock()
	bySource, ok := r.dooms[node]
	var removed bool
	if ok {
		if _, removed = bySource[source]; removed {
			delete(bySource, source)
			if len(bySource) == 0 {
				delete(r.dooms, node)
			}
		}
	}
	r.mu.Unlock()
	if removed {
		r.notify(node)
	}
}

func (r *Registry) Lookup(node string) (NodeDoom, bool) {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	bySource, ok := r.dooms[node]
	if !ok || len(bySource) == 0 {
		return NodeDoom{}, false
	}
	var best NodeDoom
	first := true
	for _, d := range bySource {
		if d.expired(now) {
			continue
		}
		if first || moreUrgent(d, best) {
			best = d
			first = false
		}
	}
	if first {
		return NodeDoom{}, false
	}
	return best, true
}

func (r *Registry) StartExpiryLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sweep(time.Now())
			}
		}
	}()
}

func (r *Registry) sweep(now time.Time) {
	var affected []string
	r.mu.Lock()
	for node, bySource := range r.dooms {
		for source, d := range bySource {
			if d.expired(now) {
				delete(bySource, source)
				affected = append(affected, node)
			}
		}
		if len(bySource) == 0 {
			delete(r.dooms, node)
		}
	}
	r.mu.Unlock()
	for _, node := range affected {
		r.notify(node)
	}
}

func (r *Registry) Subscribe(buffer int) <-chan event.GenericEvent {
	ch := make(chan event.GenericEvent, buffer)
	r.mu.Lock()
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	return ch
}

func (r *Registry) notify(node string) {
	ev := event.GenericEvent{Object: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}}
	r.mu.RLock()
	subs := r.subs
	r.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func equalDoom(a, b NodeDoom) bool {
	if a.Node != b.Node || a.Class != b.Class || a.Source != b.Source {
		return false
	}
	switch {
	case a.Deadline == nil && b.Deadline == nil:
		return true
	case a.Deadline == nil || b.Deadline == nil:
		return false
	default:
		return a.Deadline.Equal(*b.Deadline)
	}
}

func moreUrgent(a, b NodeDoom) bool {
	if a.Class != b.Class {
		return a.Class == Involuntary
	}
	if a.Class == Involuntary && a.Deadline != nil && b.Deadline != nil {
		return a.Deadline.Before(*b.Deadline)
	}
	return false
}
