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

package taint

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kylan11/understudy/internal/signal"
)

var DefaultKeys = []string{
	"karpenter.sh/disrupted",
	"ToBeDeletedByClusterAutoscaler",
	"DeletionCandidateOfClusterAutoscaler",
}

const (
	SourcePrefix      = "taint:"
	DoomKey           = "understudy.sh/doomed"
	RebalanceKey      = "understudy.sh/rebalance"
	rebalanceLifetime = 10 * time.Minute
)

type Adapter struct {
	client   client.Client
	registry *signal.Registry
	keys     []string
}

func New(keys []string) *Adapter {
	if len(keys) == 0 {
		keys = DefaultKeys
	}
	return &Adapter{keys: keys}
}

func (a *Adapter) Name() string { return "taint" }

func (a *Adapter) Keys() []string { return a.keys }

func (a *Adapter) watchKeys() []string {
	return append([]string{DoomKey, RebalanceKey}, a.keys...)
}

func (a *Adapter) doomFor(node *corev1.Node, key string) signal.NodeDoom {
	d := signal.NodeDoom{
		Node:   node.Name,
		Class:  signal.VoluntaryUnbounded,
		Source: SourcePrefix + key,
	}
	switch key {
	case DoomKey:
		d.Class = signal.Involuntary
		if v := taintValue(node, key); v != "" {
			if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
				t := time.Unix(secs, 0)
				d.Deadline = &t
			}
		}
	case RebalanceKey:
		exp := time.Now().Add(rebalanceLifetime)
		d.ExpiresAt = &exp
	}
	return d
}

func (a *Adapter) SetupWithManager(mgr ctrl.Manager, reg *signal.Registry) error {
	a.client = mgr.GetClient()
	a.registry = reg
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("signal-taint").
		// Every replica serves the webhook from its own registry, and this only writes to it.
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return true },
			DeleteFunc: func(e event.DeleteEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldNode, okOld := e.ObjectOld.(*corev1.Node)
				newNode, okNew := e.ObjectNew.(*corev1.Node)
				if !okOld || !okNew {
					return true
				}
				for _, key := range a.watchKeys() {
					if taintValue(oldNode, key) != taintValue(newNode, key) ||
						hasTaint(oldNode, key) != hasTaint(newNode, key) {
						return true
					}
				}
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool { return true },
		}).
		Complete(a)
}

func (a *Adapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := a.client.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			for _, key := range a.watchKeys() {
				a.registry.Clear(req.Name, SourcePrefix+key)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	for _, key := range a.watchKeys() {
		source := SourcePrefix + key
		if hasTaint(&node, key) {
			a.registry.Set(a.doomFor(&node, key))
		} else {
			a.registry.Clear(node.Name, source)
		}
	}
	return ctrl.Result{}, nil
}

func hasTaint(node *corev1.Node, key string) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

func taintValue(node *corev1.Node, key string) string {
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return t.Value
		}
	}
	return ""
}
