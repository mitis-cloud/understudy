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

package evictionwebhook

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1alpha1 "github.com/kylan11/understudy/api/v1alpha1"
	"github.com/kylan11/understudy/internal/metrics"
	"github.com/kylan11/understudy/internal/signal"
)

const (
	Path                   = "/observe-eviction"
	SourcePrefix           = "eviction:"
	baseReplicasAnnotation = "understudy.sh/base-replicas"
	doomLifetime           = 90 * time.Second
)

type Adapter struct {
	DoomLifetime time.Duration

	reader   client.Reader
	registry *signal.Registry
}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) lifetime() time.Duration {
	if a.DoomLifetime > 0 {
		return a.DoomLifetime
	}
	return doomLifetime
}

func (a *Adapter) Name() string { return "eviction-webhook" }

func (a *Adapter) SetupWithManager(mgr ctrl.Manager, reg *signal.Registry) error {
	a.reader = mgr.GetClient()
	a.registry = reg
	mgr.GetWebhookServer().Register(Path, &webhook.Admission{Handler: a})
	return nil
}

func (a *Adapter) Handle(ctx context.Context, req admission.Request) admission.Response {
	allowed := admission.Allowed("")
	if req.DryRun != nil && *req.DryRun {
		return allowed
	}
	if req.Name == "" || req.Namespace == "" {
		return allowed
	}

	var pod corev1.Pod
	if err := a.reader.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pod); err != nil {
		return allowed
	}
	if pod.Spec.NodeName == "" {
		return allowed
	}

	cr, dep, protected := a.resolveProtector(ctx, &pod)
	metrics.EvictionAttemptsObserved.WithLabelValues(strconv.FormatBool(protected)).Inc()
	if !protected {
		return allowed
	}

	expires := time.Now().Add(a.lifetime())
	a.registry.Set(signal.NodeDoom{
		Node:      pod.Spec.NodeName,
		Class:     signal.VoluntaryUnbounded,
		ExpiresAt: &expires,
		Source:    SourcePrefix + req.UserInfo.Username,
	})
	logf.FromContext(ctx).V(1).Info("eviction attempt observed",
		"pod", req.Namespace+"/"+req.Name, "node", pod.Spec.NodeName, "evictor", req.UserInfo.Username)

	if !cr.ExperimentalEvictionHold() {
		return allowed
	}
	if cr.HostageModeOrDefault() == appsv1alpha1.HostageOff {
		return allowed
	}
	if cr.Status.Phase == appsv1alpha1.PhaseRelaxed {
		return allowed
	}
	if a.standInReady(ctx, dep) {
		return allowed
	}
	metrics.EvictionsHeld.Inc()
	logf.FromContext(ctx).Info("eviction held; stand-in not ready",
		"pod", req.Namespace+"/"+req.Name, "evictor", req.UserInfo.Username)
	return admission.Response{AdmissionResponse: admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusTooManyRequests,
			Reason:  metav1.StatusReasonTooManyRequests,
			Message: fmt.Sprintf("understudy is preparing a stand-in for %s/%s; retry shortly", req.Namespace, req.Name),
		},
	}}
}

func (a *Adapter) resolveProtector(ctx context.Context, pod *corev1.Pod) (*appsv1alpha1.Understudy, *appsv1.Deployment, bool) {
	var crs appsv1alpha1.UnderstudyList
	if err := a.reader.List(ctx, &crs, client.InNamespace(pod.Namespace)); err != nil {
		return nil, nil, false
	}
	for i := range crs.Items {
		cr := &crs.Items[i]
		if kind := cr.Spec.TargetRef.Kind; kind != "" && kind != "Deployment" {
			continue
		}
		var dep appsv1.Deployment
		if err := a.reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: cr.Spec.TargetRef.Name}, &dep); err != nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil {
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			return cr, &dep, true
		}
	}
	return nil, nil, false
}

func (a *Adapter) standInReady(ctx context.Context, dep *appsv1.Deployment) bool {
	base := int32(1)
	if dep.Spec.Replicas != nil {
		base = *dep.Spec.Replicas
	}
	if v, ok := dep.Annotations[baseReplicasAnnotation]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil {
			base = int32(parsed)
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return false
	}
	var pods corev1.PodList
	if err := a.reader.List(ctx, &pods, client.InNamespace(dep.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false
	}
	healthy := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				healthy++
				break
			}
		}
	}
	return healthy > int(base)
}
