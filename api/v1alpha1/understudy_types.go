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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:Enum=always;voluntary-only;off
type HostageMode string

const (
	HostageAlways        HostageMode = "always"
	HostageVoluntaryOnly HostageMode = "voluntary-only"
	HostageOff           HostageMode = "off"
)

type Phase string

const (
	PhaseIdle        Phase = "Idle"
	PhaseSurging     Phase = "Surging"
	PhaseReleasing   Phase = "Releasing"
	PhaseRelaxed     Phase = "Relaxed"
	PhaseUnsupported Phase = "Unsupported"
)

const (
	ConditionTargetSupported      = "TargetSupported"
	ConditionProtected            = "Protected"
	ConditionSurgeReady           = "SurgeReady"
	ConditionZeroDowntimeStrategy = "ZeroDowntimeUpdateStrategy"
)

type TargetReference struct {
	// +kubebuilder:default=Deployment
	// +optional
	Kind string `json:"kind,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

type UnderstudySpec struct {
	// +required
	TargetRef TargetReference `json:"targetRef"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	SurgeReplicas *int32 `json:"surgeReplicas,omitempty"`

	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	ReadinessDeadlineSeconds *int32 `json:"readinessDeadlineSeconds,omitempty"`

	// +kubebuilder:default=always
	// +optional
	HostageMode HostageMode `json:"hostageMode,omitempty"`

	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinSurgeTimeSeconds *int32 `json:"minSurgeTimeSeconds,omitempty"`
}

type UnderstudyStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// +optional
	LastSurgeTime *metav1.Time `json:"lastSurgeTime,omitempty"`

	// +optional
	LastReleaseTime *metav1.Time `json:"lastReleaseTime,omitempty"`

	// +optional
	BlockedSince *metav1.Time `json:"blockedSince,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.hostageMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Blocked-Since",type=date,JSONPath=`.status.blockedSince`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Understudy struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec UnderstudySpec `json:"spec"`

	// +optional
	Status UnderstudyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

type UnderstudyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Understudy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Understudy{}, &UnderstudyList{})
		return nil
	})
}

func (u *Understudy) SurgeReplicasOrDefault() int32 {
	if u.Spec.SurgeReplicas != nil {
		return *u.Spec.SurgeReplicas
	}
	return 1
}

func (u *Understudy) ReadinessDeadlineOrDefault() int32 {
	if u.Spec.ReadinessDeadlineSeconds != nil {
		return *u.Spec.ReadinessDeadlineSeconds
	}
	return 600
}

func (u *Understudy) HostageModeOrDefault() HostageMode {
	if u.Spec.HostageMode == "" {
		return HostageAlways
	}
	return u.Spec.HostageMode
}

func (u *Understudy) MinSurgeTimeOrDefault() int32 {
	if u.Spec.MinSurgeTimeSeconds != nil {
		return *u.Spec.MinSurgeTimeSeconds
	}
	return 60
}
