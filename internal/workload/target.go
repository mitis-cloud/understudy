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

package workload

import (
	"context"
	"fmt"
	"strconv"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationBaseReplicas   = "understudy.sh/base-replicas"
	AnnotationTargetRevision = "understudy.sh/target-revision"
	KindDeployment           = "Deployment"
	KindRollout              = "Rollout"
)

type Target struct {
	client.Object
}

func Supported(kind string) bool {
	return kind == "" || kind == KindDeployment || kind == KindRollout
}

func Resolve(ctx context.Context, reader client.Reader, key client.ObjectKey, kind string) (*Target, error) {
	var obj client.Object
	switch kind {
	case "", KindDeployment:
		obj = &appsv1.Deployment{}
	case KindRollout:
		obj = &rolloutsv1alpha1.Rollout{}
	default:
		return nil, fmt.Errorf("unsupported target kind %q", kind)
	}
	if err := reader.Get(ctx, key, obj); err != nil {
		return nil, err
	}
	return &Target{Object: obj}, nil
}

func (t *Target) Kind() string {
	if _, ok := t.Object.(*rolloutsv1alpha1.Rollout); ok {
		return KindRollout
	}
	return KindDeployment
}

func (t *Target) BaseReplicas() (int32, bool) {
	if v, ok := t.GetAnnotations()[AnnotationBaseReplicas]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil {
			return int32(parsed), true
		}
	}
	return t.Replicas(), false
}

func (t *Target) Replicas() int32 {
	var replicas *int32
	switch obj := t.Object.(type) {
	case *appsv1.Deployment:
		replicas = obj.Spec.Replicas
	case *rolloutsv1alpha1.Rollout:
		replicas = obj.Spec.Replicas
	}
	if replicas == nil {
		return 1
	}
	return *replicas
}

// PodAvailable applies the workload controller's own availability rule: Ready
// for longer than the target's minReadySeconds.
func (t *Target) PodAvailable(pod *corev1.Pod, now time.Time) bool {
	minReady := time.Duration(t.minReadySeconds()) * time.Second
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue &&
				(minReady == 0 || !c.LastTransitionTime.IsZero() && c.LastTransitionTime.Add(minReady).Before(now))
		}
	}
	return false
}

func (t *Target) minReadySeconds() int32 {
	switch obj := t.Object.(type) {
	case *appsv1.Deployment:
		return obj.Spec.MinReadySeconds
	case *rolloutsv1alpha1.Rollout:
		return obj.Spec.MinReadySeconds
	}
	return 0
}

func (t *Target) revision() string {
	if ro, ok := t.Object.(*rolloutsv1alpha1.Rollout); ok {
		return ro.Status.CurrentPodHash
	}
	return t.GetAnnotations()["deployment.kubernetes.io/revision"]
}

func (t *Target) Selector() (labels.Selector, error) {
	var selector *metav1.LabelSelector
	switch obj := t.Object.(type) {
	case *appsv1.Deployment:
		selector = obj.Spec.Selector
	case *rolloutsv1alpha1.Rollout:
		selector = obj.Spec.Selector
	}
	return metav1.LabelSelectorAsSelector(selector)
}

func (t *Target) ServingSelector() (labels.Selector, error) {
	selector, err := t.Selector()
	if err != nil {
		return nil, err
	}
	if ro, ok := t.Object.(*rolloutsv1alpha1.Rollout); ok {
		hash := ro.Status.StableRS
		if ro.Spec.Strategy.BlueGreen != nil {
			hash = ro.Status.BlueGreen.ActiveSelector
		}
		if hash == "" {
			return labels.Nothing(), nil
		}
		serving := labels.SelectorFromSet(labels.Set{rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: hash})
		requirements, _ := serving.Requirements()
		selector = selector.Add(requirements...)
	}
	return selector, nil
}

func (t *Target) PatchReplicas(ctx context.Context, c client.Client, replicas int32, base *int32) error {
	patched := t.DeepCopyObject().(client.Object)
	annotations := patched.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if base == nil {
		delete(annotations, AnnotationBaseReplicas)
		delete(annotations, AnnotationTargetRevision)
	} else {
		annotations[AnnotationBaseReplicas] = strconv.FormatInt(int64(*base), 10)
		if _, active := t.BaseReplicas(); !active && t.revision() != "" {
			annotations[AnnotationTargetRevision] = t.revision()
		}
	}
	patched.SetAnnotations(annotations)
	switch obj := patched.(type) {
	case *appsv1.Deployment:
		obj.Spec.Replicas = &replicas
	case *rolloutsv1alpha1.Rollout:
		obj.Spec.Replicas = &replicas
	}
	return c.Patch(ctx, patched, client.MergeFromWithOptions(t.Object, client.MergeFromWithOptimisticLock{}))
}

func (t *Target) StandDown() bool {
	base, surging := t.BaseReplicas()
	if revision := t.GetAnnotations()[AnnotationTargetRevision]; surging && revision != "" && revision != t.revision() {
		return true
	}
	switch obj := t.Object.(type) {
	case *appsv1.Deployment:
		if surging {
			return false
		}
		if obj.Status.ObservedGeneration < obj.Generation {
			return true
		}
		for _, c := range obj.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionTrue && c.Reason != "NewReplicaSetAvailable" {
				return true
			}
		}
		return obj.Status.UpdatedReplicas < base
	case *rolloutsv1alpha1.Rollout:
		// Our replica patch also increments generation; it is not a new release.
		if !surging && obj.Status.ObservedGeneration != strconv.FormatInt(obj.Generation, 10) {
			return true
		}
		if obj.Spec.Paused || obj.Status.ControllerPause || len(obj.Status.PauseConditions) > 0 || obj.Status.Abort {
			return true
		}
		if obj.Status.StableRS == "" || obj.Status.StableRS != obj.Status.CurrentPodHash {
			return true
		}
		if bg := obj.Spec.Strategy.BlueGreen; bg != nil {
			return obj.Status.BlueGreen.ActiveSelector != obj.Status.CurrentPodHash
		}
		if canary := obj.Spec.Strategy.Canary; canary != nil {
			return len(canary.Steps) > 0 && (obj.Status.CurrentStepIndex == nil || int(*obj.Status.CurrentStepIndex) < len(canary.Steps))
		}
		return true
	}
	return true
}

func (t *Target) ZeroDowntimeStrategy(base int32) (bool, string, string) {
	var unavailable, surge *intstr.IntOrString
	switch obj := t.Object.(type) {
	case *appsv1.Deployment:
		if obj.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
			return false, "Recreate", "strategy Recreate drops all traffic on every deploy"
		}
		if ru := obj.Spec.Strategy.RollingUpdate; ru != nil {
			unavailable, surge = ru.MaxUnavailable, ru.MaxSurge
		}
	case *rolloutsv1alpha1.Rollout:
		if obj.Spec.Strategy.BlueGreen != nil {
			return true, "BlueGreenStrategy", "blue/green promotion keeps the active ReplicaSet serving"
		}
		if canary := obj.Spec.Strategy.Canary; canary != nil {
			unavailable, surge = canary.MaxUnavailable, canary.MaxSurge
		}
	}
	defaultPercentage := intstr.FromString("25%")
	if unavailable == nil {
		unavailable = &defaultPercentage
	}
	if surge == nil {
		surge = &defaultPercentage
	}
	maxUnavailable, _ := intstr.GetScaledValueFromIntOrPercent(unavailable, int(base), false)
	maxSurge, _ := intstr.GetScaledValueFromIntOrPercent(surge, int(base), true)
	if maxUnavailable > 0 || maxSurge < 1 {
		return false, "LossyStrategy", fmt.Sprintf("resolved maxUnavailable=%d, maxSurge=%d: deploys can drop traffic; set maxUnavailable: 0, maxSurge: 1", maxUnavailable, maxSurge)
	}
	return true, "SurgingStrategy", "rolling updates keep capacity during deploys"
}
