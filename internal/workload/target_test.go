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
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestRolloutStandDown(t *testing.T) {
	const stable = "stable"
	tests := []struct {
		name   string
		mutate func(*rolloutsv1alpha1.Rollout)
		want   bool
	}{
		{"settled", func(_ *rolloutsv1alpha1.Rollout) {}, false},
		{"unobserved release", func(ro *rolloutsv1alpha1.Rollout) { ro.Generation++ }, true},
		{"own surge awaiting observation", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Generation++
			ro.Annotations = map[string]string{AnnotationBaseReplicas: "1"}
		}, false},
		{"spec pause", func(ro *rolloutsv1alpha1.Rollout) { ro.Spec.Paused = true }, true},
		{"controller pause", func(ro *rolloutsv1alpha1.Rollout) { ro.Status.ControllerPause = true }, true},
		{"pause condition", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Status.PauseConditions = []rolloutsv1alpha1.PauseCondition{{Reason: rolloutsv1alpha1.PauseReasonCanaryPauseStep}}
		}, true},
		{"abort", func(ro *rolloutsv1alpha1.Rollout) { ro.Status.Abort = true }, true},
		{"unknown stable revision", func(ro *rolloutsv1alpha1.Rollout) { ro.Status.StableRS = "" }, true},
		{"new revision", func(ro *rolloutsv1alpha1.Rollout) { ro.Status.CurrentPodHash = "new" }, true},
		{"unexecuted steps", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Spec.Strategy.Canary.Steps = []rolloutsv1alpha1.CanaryStep{{SetWeight: ptr.To(int32(100))}}
		}, true},
		{"completed steps", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Spec.Strategy.Canary.Steps = []rolloutsv1alpha1.CanaryStep{{SetWeight: ptr.To(int32(100))}}
			ro.Status.CurrentStepIndex = ptr.To(int32(1))
		}, false},
		{"blue green serving old revision", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Spec.Strategy = rolloutsv1alpha1.RolloutStrategy{BlueGreen: &rolloutsv1alpha1.BlueGreenStrategy{}}
			ro.Status.BlueGreen.ActiveSelector = "old"
		}, true},
		{"blue green promoted", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Spec.Strategy = rolloutsv1alpha1.RolloutStrategy{BlueGreen: &rolloutsv1alpha1.BlueGreenStrategy{}}
			ro.Status.BlueGreen.ActiveSelector = stable
		}, false},
		{"unavailable pods without a release", func(ro *rolloutsv1alpha1.Rollout) {
			ro.Status.UpdatedReplicas = 0
			ro.Status.AvailableReplicas = 0
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro := &rolloutsv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Spec: rolloutsv1alpha1.RolloutSpec{Strategy: rolloutsv1alpha1.RolloutStrategy{
					Canary: &rolloutsv1alpha1.CanaryStrategy{},
				}},
				Status: rolloutsv1alpha1.RolloutStatus{ObservedGeneration: "1", StableRS: stable, CurrentPodHash: stable},
			}
			tt.mutate(ro)
			if got := (&Target{Object: ro}).StandDown(); got != tt.want {
				t.Fatalf("StandDown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodAvailable(t *testing.T) {
	now := time.Now()
	ready := func(status corev1.ConditionStatus, since time.Time) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(since),
		}}}}
	}
	tests := []struct {
		name            string
		pod             *corev1.Pod
		minReadySeconds int32
		want            bool
	}{
		{"no ready condition", &corev1.Pod{}, 0, false},
		{"not ready", ready(corev1.ConditionFalse, now.Add(-time.Hour)), 0, false},
		{"ready without minReadySeconds", ready(corev1.ConditionTrue, now), 0, true},
		{"ready for less than minReadySeconds", ready(corev1.ConditionTrue, now.Add(-29*time.Second)), 30, false},
		{"ready for exactly minReadySeconds", ready(corev1.ConditionTrue, now.Add(-30*time.Second)), 30, false},
		{"ready for longer than minReadySeconds", ready(corev1.ConditionTrue, now.Add(-31*time.Second)), 30, true},
		{"ready since an unknown time", ready(corev1.ConditionTrue, time.Time{}), 30, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, target := range []*Target{
				{Object: &appsv1.Deployment{Spec: appsv1.DeploymentSpec{MinReadySeconds: tt.minReadySeconds}}},
				{Object: &rolloutsv1alpha1.Rollout{Spec: rolloutsv1alpha1.RolloutSpec{MinReadySeconds: tt.minReadySeconds}}},
			} {
				if got := target.PodAvailable(tt.pod, now); got != tt.want {
					t.Errorf("%s PodAvailable() = %v, want %v", target.Kind(), got, tt.want)
				}
			}
		})
	}
}
