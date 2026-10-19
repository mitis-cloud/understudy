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

package controller

import (
	"context"
	"fmt"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	appsv1alpha1 "github.com/kylan11/understudy/api/v1alpha1"
	"github.com/kylan11/understudy/internal/metrics"
	"github.com/kylan11/understudy/internal/signal"
	"github.com/kylan11/understudy/internal/workload"
)

const (
	AnnotationBaseReplicas    = workload.AnnotationBaseReplicas
	LabelOwned                = "understudy.sh/owned"
	PodDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	doomedPodDeletionCost     = "-1000"

	relaxReasonTTL      = "ttl"
	relaxReasonDeadline = "deadline"
	relaxReasonMode     = "mode"

	activeRequeue = 5 * time.Second
)

type UnderstudyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Registry *signal.Registry

	OperatorNamespace  string
	OperatorDeployment string
}

type podAssessment struct {
	doomed    []*corev1.Pod
	worstDoom *signal.NodeDoom
	viable    int
	all       []corev1.Pod
}

// +kubebuilder:rbac:groups=apps.understudy.sh,resources=understudies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.understudy.sh,resources=understudies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.understudy.sh,resources=understudies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *UnderstudyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr appsv1alpha1.Understudy
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.UntrackBlocked(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	status := *cr.Status.DeepCopy()

	if kind := cr.Spec.TargetRef.Kind; !workload.Supported(kind) {
		return r.markUnsupported(ctx, &cr, status,
			fmt.Sprintf("target kind %q is not supported (only Deployment and Rollout); skipping", kind))
	}
	if cr.Spec.TargetRef.Kind != workload.KindRollout && cr.Namespace == r.OperatorNamespace && cr.Spec.TargetRef.Name == r.OperatorDeployment && r.OperatorDeployment != "" {
		return r.markUnsupported(ctx, &cr, status,
			"refusing to manage the Understudy operator itself (self-exclusion fail-safe)")
	}

	target, err := workload.Resolve(ctx, r.Client,
		types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}, cr.Spec.TargetRef.Kind)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return r.markUnsupported(ctx, &cr, status, "Rollout API is unavailable; install Argo Rollouts and restart Understudy")
		}
		if apierrors.IsNotFound(err) {
			setCond(&status, &cr, appsv1alpha1.ConditionTargetSupported, metav1.ConditionFalse,
				"TargetNotFound", fmt.Sprintf("target %q not found", cr.Spec.TargetRef.Name))
			status.Phase = appsv1alpha1.PhaseIdle
			return ctrl.Result{}, r.patchStatus(ctx, &cr, status)
		}
		return ctrl.Result{}, err
	}
	setCond(&status, &cr, appsv1alpha1.ConditionTargetSupported, metav1.ConditionTrue, target.Kind(), "target resolved")

	base, surgeActive := target.BaseReplicas()
	desiredBase := base
	if cr.Spec.BaselineReplicas != nil {
		desiredBase = *cr.Spec.BaselineReplicas
	}
	setBaselineCondition(&status, &cr, base)
	zeroDowntime, reason, message := target.ZeroDowntimeStrategy(base)
	setCond(&status, &cr, appsv1alpha1.ConditionZeroDowntimeStrategy, boolToStatus(zeroDowntime), reason, message)

	assessment, err := r.assessPods(ctx, &cr, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	surgeReady := assessment.viable >= int(base)

	mode := cr.HoldModeOrDefault()
	if status.Mode == "" && cr.UsesDeprecatedHoldMode() {
		r.event(&cr, corev1.EventTypeWarning, "DeprecatedField", "Configure",
			"spec.hostageMode is deprecated and will be removed; rename it to spec.holdMode")
	}
	status.Mode = mode
	if target.StandDown() {
		if surgeActive {
			if err := r.clearDeletionCosts(ctx, assessment.all); err != nil {
				return ctrl.Result{}, err
			}
		}
		if surgeActive || cr.Spec.BaselineReplicas != nil && target.Replicas() != desiredBase {
			if err := target.PatchReplicas(ctx, r.Client, desiredBase, nil); err != nil {
				return ctrl.Result{}, err
			}
		}
		setBaselineCondition(&status, &cr, desiredBase)
		status.Phase = appsv1alpha1.PhaseIdle
		status.BlockedSince = nil
		metrics.UntrackBlocked(req.String())
		setCond(&status, &cr, appsv1alpha1.ConditionProtected, metav1.ConditionFalse,
			"RolloutInProgress", "target is mid-rollout; evictions are admitted")
		setCond(&status, &cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"RolloutInProgress", "target is mid-rollout; standing down")
		r.eventOnce(&cr, &status, corev1.EventTypeNormal, "RolloutStandDown", "Skip",
			fmt.Sprintf("standing down for %s: rollout in progress", target.GetName()))
		return ctrl.Result{RequeueAfter: activeRequeue}, r.patchStatus(ctx, &cr, status)
	}
	relaxReason, ttlExpired := r.relaxDecision(&cr, mode, assessment, surgeReady, status.BlockedSince)
	setProtectedCondition(&status, &cr, mode, relaxReason)

	switch {
	case !surgeActive && (len(assessment.doomed) == 0 || base == 0):
		if cr.Spec.BaselineReplicas != nil && target.Replicas() != desiredBase {
			if err := target.PatchReplicas(ctx, r.Client, desiredBase, nil); err != nil {
				return ctrl.Result{}, err
			}
		}
		setBaselineCondition(&status, &cr, desiredBase)
		status.Phase = appsv1alpha1.PhaseIdle
		status.BlockedSince = nil
		metrics.UntrackBlocked(req.String())
		setCond(&status, &cr, appsv1alpha1.ConditionSurgeReady, boolToStatus(assessment.viable >= int(desiredBase)), "Steady", "no disruption in sight")
		return ctrl.Result{}, r.patchStatus(ctx, &cr, status)

	case len(assessment.doomed) == 0 && surgeActive:
		return r.reconcileScaleBack(ctx, &cr, target, status, assessment, desiredBase, req)

	default:
		return r.reconcileDoomed(ctx, &cr, target, status, assessment, base, surgeActive, surgeReady, relaxReason, ttlExpired, req)
	}
}

func (r *UnderstudyReconciler) assessPods(ctx context.Context, cr *appsv1alpha1.Understudy, target *workload.Target) (podAssessment, error) {
	var a podAssessment
	selector, err := target.Selector()
	if err != nil {
		return a, err
	}
	serving, err := target.ServingSelector()
	if err != nil {
		return a, err
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return a, err
	}
	a.all = podList.Items
	now := time.Now()
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !serving.Matches(labels.Set(pod.Labels)) || pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}
		if doom, doomed := r.Registry.Lookup(pod.Spec.NodeName); doomed {
			a.doomed = append(a.doomed, pod)
			if a.worstDoom == nil || doom.Class == signal.Involuntary {
				d := doom
				a.worstDoom = &d
			}
			continue
		}
		if target.PodAvailable(pod, now) {
			a.viable++
		}
	}
	return a, nil
}

func (r *UnderstudyReconciler) relaxDecision(cr *appsv1alpha1.Understudy, mode appsv1alpha1.HoldMode,
	a podAssessment, surgeReady bool, blockedSince *metav1.Time) (string, bool) {
	relaxReason := ""
	if a.worstDoom != nil && a.worstDoom.Class == signal.Involuntary {
		if mode == appsv1alpha1.HoldVoluntaryOnly {
			relaxReason = relaxReasonMode
		} else if a.worstDoom.Deadline != nil && !surgeReady &&
			time.Until(*a.worstDoom.Deadline) < time.Duration(cr.MinSurgeTimeOrDefault())*time.Second {
			relaxReason = relaxReasonDeadline
		}
	}
	ttlExpired := false
	if len(a.doomed) > 0 && !surgeReady && blockedSince != nil {
		deadline := blockedSince.Add(time.Duration(cr.ReadinessDeadlineOrDefault()) * time.Second)
		if time.Now().After(deadline) {
			ttlExpired = true
			if relaxReason == "" {
				relaxReason = relaxReasonTTL
			}
		}
	}
	return relaxReason, ttlExpired
}

func (r *UnderstudyReconciler) reconcileScaleBack(ctx context.Context, cr *appsv1alpha1.Understudy,
	target *workload.Target, status appsv1alpha1.UnderstudyStatus, a podAssessment, base int32,
	req ctrl.Request) (ctrl.Result, error) {
	if err := r.clearDeletionCosts(ctx, a.all); err != nil {
		return ctrl.Result{}, err
	}
	if err := target.PatchReplicas(ctx, r.Client, base, nil); err != nil {
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	status.LastReleaseTime = &now
	status.BlockedSince = nil
	status.Phase = appsv1alpha1.PhaseIdle
	setBaselineCondition(&status, cr, base)
	metrics.UntrackBlocked(req.String())
	r.event(cr, corev1.EventTypeNormal, "ScaledBack", "Release",
		fmt.Sprintf("predicament over; scaled %s back to %d replicas", target.GetName(), base))
	logf.FromContext(ctx).Info("Scaled back workload", "kind", target.Kind(), "name", target.GetName(), "replicas", base)
	return ctrl.Result{}, r.patchStatus(ctx, cr, status)
}

func (r *UnderstudyReconciler) reconcileDoomed(ctx context.Context, cr *appsv1alpha1.Understudy,
	target *workload.Target, status appsv1alpha1.UnderstudyStatus, a podAssessment, base int32,
	surgeActive, surgeReady bool, relaxReason string, ttlExpired bool, req ctrl.Request) (ctrl.Result, error) {

	if status.BlockedSince == nil {
		now := metav1.Now()
		status.BlockedSince = &now
	}
	metrics.TrackBlocked(req.String(), status.BlockedSince.Time)

	// A failed surge still reaches the status patch below: the hold must be
	// recorded for readinessDeadlineSeconds to be able to relax it.
	var surgeErr error
	if !surgeActive || cr.Spec.BaselineReplicas != nil && target.Replicas() != base+cr.SurgeReplicasOrDefault() {
		surgeErr = target.PatchReplicas(ctx, r.Client, base+cr.SurgeReplicasOrDefault(), &base)
	}
	if surgeErr == nil && !surgeActive {
		now := metav1.Now()
		status.LastSurgeTime = &now
		class := signal.VoluntaryUnbounded
		if a.worstDoom != nil {
			class = a.worstDoom.Class
		}
		metrics.SurgesTotal.WithLabelValues(string(class)).Inc()
		r.event(cr, corev1.EventTypeNormal, "SurgeStarted", "Surge",
			fmt.Sprintf("node doom (%s) for pod(s) of %s; surging %d -> %d",
				doomSource(a.worstDoom), target.GetName(), base, base+cr.SurgeReplicasOrDefault()))
	}
	if surgeErr == nil {
		surgeErr = r.stampDeletionCosts(ctx, a.doomed)
	}
	if surgeErr != nil {
		logf.FromContext(ctx).Error(surgeErr, "Failed to surge workload", "kind", target.Kind(), "name", target.GetName())
	}

	previousPhase := status.Phase
	switch {
	case surgeReady:
		status.Phase = appsv1alpha1.PhaseReleasing
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionTrue,
			"UnderstudyReady", "understudy is traffic-ready; eviction may proceed")
		r.eventOnce(cr, &status, corev1.EventTypeNormal, "SurgeReady", "Release",
			fmt.Sprintf("understudy for %s is traffic-ready; releasing the eviction", target.GetName()))
	case relaxReason != "" || ttlExpired:
		status.Phase = appsv1alpha1.PhaseRelaxed
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"Relaxed", "eviction released without a ready understudy ("+relaxReason+")")
		if status.LastReleaseTime == nil || status.LastSurgeTime != nil && status.LastReleaseTime.Before(status.LastSurgeTime) {
			now := metav1.Now()
			status.LastReleaseTime = &now
		}
		if previousPhase != appsv1alpha1.PhaseRelaxed {
			metrics.RelaxedTotal.WithLabelValues(relaxReason).Inc()
		}
		r.eventOnce(cr, &status, corev1.EventTypeWarning, "HoldRelaxed", "Relax",
			fmt.Sprintf("eviction hold for %s relaxed (%s); eviction can proceed without a ready understudy", target.GetName(), relaxReason))
	case surgeErr != nil:
		status.Phase = appsv1alpha1.PhaseSurging
		message := fmt.Sprintf("cannot surge %s: %v", target.GetName(), surgeErr)
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse, "SurgeFailed", message)
		r.eventOnce(cr, &status, corev1.EventTypeWarning, "SurgeFailed", "Surge", message)
	default:
		status.Phase = appsv1alpha1.PhaseSurging
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"AwaitingReadiness", fmt.Sprintf("waiting for %d ready replica(s) on healthy nodes (have %d)", base, a.viable))
	}
	return ctrl.Result{RequeueAfter: activeRequeue}, r.patchStatus(ctx, cr, status)
}

func (r *UnderstudyReconciler) stampDeletionCosts(ctx context.Context, pods []*corev1.Pod) error {
	for _, pod := range pods {
		if pod.Annotations[PodDeletionCostAnnotation] == doomedPodDeletionCost {
			continue
		}
		patched := pod.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		patched.Annotations[PodDeletionCostAnnotation] = doomedPodDeletionCost
		if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *UnderstudyReconciler) clearDeletionCosts(ctx context.Context, pods []corev1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		if _, ok := pod.Annotations[PodDeletionCostAnnotation]; !ok {
			continue
		}
		patched := pod.DeepCopy()
		delete(patched.Annotations, PodDeletionCostAnnotation)
		if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *UnderstudyReconciler) markUnsupported(ctx context.Context, cr *appsv1alpha1.Understudy,
	status appsv1alpha1.UnderstudyStatus, msg string) (ctrl.Result, error) {
	alreadyMarked := status.Phase == appsv1alpha1.PhaseUnsupported
	status.Phase = appsv1alpha1.PhaseUnsupported
	setCond(&status, cr, appsv1alpha1.ConditionTargetSupported, metav1.ConditionFalse, "UnsupportedTarget", msg)
	if !alreadyMarked {
		r.event(cr, corev1.EventTypeWarning, "UnsupportedTarget", "Skip", msg)
	}
	return ctrl.Result{}, r.patchStatus(ctx, cr, status)
}

func setProtectedCondition(status *appsv1alpha1.UnderstudyStatus, cr *appsv1alpha1.Understudy,
	mode appsv1alpha1.HoldMode, relaxReason string) {
	reason, msg := "HoldOff", "holdMode is off; evictions are admitted immediately"
	if mode != appsv1alpha1.HoldOff {
		reason, msg = "EvictionHold", "evictions are held at admission while a stand-in is prepared"
		if relaxReason != "" {
			reason, msg = "Relaxed", "eviction hold relaxed ("+relaxReason+"); evictions may proceed"
		}
	}
	setCond(status, cr, appsv1alpha1.ConditionProtected, boolToStatus(mode != appsv1alpha1.HoldOff && relaxReason == ""), reason, msg)
}

func setBaselineCondition(status *appsv1alpha1.UnderstudyStatus, cr *appsv1alpha1.Understudy, base int32) {
	if cr.Spec.BaselineReplicas == nil {
		status.BaselineReplicas = nil
		meta.RemoveStatusCondition(&status.Conditions, appsv1alpha1.ConditionBaselineApplied)
		return
	}
	status.BaselineReplicas = ptr.To(base)
	if *cr.Spec.BaselineReplicas != base {
		setCond(status, cr, appsv1alpha1.ConditionBaselineApplied, metav1.ConditionFalse,
			"HandoverInProgress", fmt.Sprintf("baseline %d is pending; handover retains baseline %d", *cr.Spec.BaselineReplicas, base))
		return
	}
	setCond(status, cr, appsv1alpha1.ConditionBaselineApplied, metav1.ConditionTrue,
		"Applied", fmt.Sprintf("baseline %d is applied; effective replicas may include a temporary surge", base))
}

func (r *UnderstudyReconciler) patchStatus(ctx context.Context, cr *appsv1alpha1.Understudy, status appsv1alpha1.UnderstudyStatus) error {
	if statusEqual(&cr.Status, &status) {
		return nil
	}
	patched := cr.DeepCopy()
	patched.Status = status
	return r.Status().Patch(ctx, patched, client.MergeFrom(cr))
}

func (r *UnderstudyReconciler) event(cr *appsv1alpha1.Understudy, eventType, reason, action, note string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(cr, nil, eventType, reason, action, "%s", note)
}

func (r *UnderstudyReconciler) eventOnce(cr *appsv1alpha1.Understudy, status *appsv1alpha1.UnderstudyStatus,
	eventType, reason, action, note string) {
	prev := meta.FindStatusCondition(cr.Status.Conditions, appsv1alpha1.ConditionSurgeReady)
	next := meta.FindStatusCondition(status.Conditions, appsv1alpha1.ConditionSurgeReady)
	if prev != nil && next != nil && prev.Status == next.Status && prev.Reason == next.Reason {
		return
	}
	r.event(cr, eventType, reason, action, note)
}

func (r *UnderstudyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapDeployment := func(ctx context.Context, obj client.Object) []ctrl.Request {
		return r.requestsForTarget(ctx, obj.GetNamespace(), obj.GetName(), workload.KindDeployment)
	}
	mapRollout := func(ctx context.Context, obj client.Object) []ctrl.Request {
		return r.requestsForTarget(ctx, obj.GetNamespace(), obj.GetName(), workload.KindRollout)
	}
	mapPod := func(ctx context.Context, obj client.Object) []ctrl.Request {
		return r.requestsInNamespace(ctx, obj.GetNamespace())
	}
	mapDoom := func(ctx context.Context, _ client.Object) []ctrl.Request {
		return r.requestsInNamespace(ctx, "")
	}

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Understudy{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(mapDeployment)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPod)).
		WatchesRawSource(source.Channel(r.Registry.Subscribe(64), handler.EnqueueRequestsFromMapFunc(mapDoom))).
		Named("understudy")
	gvk := rolloutsv1alpha1.SchemeGroupVersion.WithKind(workload.KindRollout)
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
		builder = builder.Watches(&rolloutsv1alpha1.Rollout{}, handler.EnqueueRequestsFromMapFunc(mapRollout))
	} else if !meta.IsNoMatchError(err) {
		return err
	} else {
		mgr.GetLogger().Info("Rollout API is unavailable; watching Deployments only")
	}
	return builder.Complete(r)
}

func (r *UnderstudyReconciler) requestsForTarget(ctx context.Context, namespace, name, kind string) []ctrl.Request {
	var list appsv1alpha1.UnderstudyList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		ref := list.Items[i].Spec.TargetRef
		if ref.Kind == "" {
			ref.Kind = workload.KindDeployment
		}
		if ref.Name == name && ref.Kind == kind {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func (r *UnderstudyReconciler) requestsInNamespace(ctx context.Context, namespace string) []ctrl.Request {
	var list appsv1alpha1.UnderstudyList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func setCond(status *appsv1alpha1.UnderstudyStatus, cr *appsv1alpha1.Understudy,
	condType string, condStatus metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cr.Generation,
	})
}

func boolToStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func statusEqual(a, b *appsv1alpha1.UnderstudyStatus) bool {
	if !ptr.Equal(a.BaselineReplicas, b.BaselineReplicas) || a.Phase != b.Phase || a.Mode != b.Mode || !timePtrEqual(a.LastSurgeTime, b.LastSurgeTime) ||
		!timePtrEqual(a.LastReleaseTime, b.LastReleaseTime) || !timePtrEqual(a.BlockedSince, b.BlockedSince) ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ca, cb := a.Conditions[i], b.Conditions[i]
		if ca.Type != cb.Type || ca.Status != cb.Status || ca.Reason != cb.Reason || ca.Message != cb.Message || ca.ObservedGeneration != cb.ObservedGeneration {
			return false
		}
	}
	return true
}

func timePtrEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

func doomSource(d *signal.NodeDoom) string {
	if d == nil {
		return "unknown"
	}
	return d.Source
}
