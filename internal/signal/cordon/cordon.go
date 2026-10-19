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

package cordon

import (
	"context"

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

const Source = "cordon"

type Adapter struct {
	client   client.Client
	registry *signal.Registry
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "cordon" }

func (a *Adapter) SetupWithManager(mgr ctrl.Manager, reg *signal.Registry) error {
	a.client = mgr.GetClient()
	a.registry = reg
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("signal-cordon").
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
				return oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable
			},
			GenericFunc: func(e event.GenericEvent) bool { return true },
		}).
		Complete(a)
}

func (a *Adapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := a.client.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			a.registry.Clear(req.Name, Source)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if node.Spec.Unschedulable {
		a.registry.Set(signal.NodeDoom{
			Node:   node.Name,
			Class:  signal.VoluntaryUnbounded,
			Source: Source,
		})
	} else {
		a.registry.Clear(node.Name, Source)
	}
	return ctrl.Result{}, nil
}
