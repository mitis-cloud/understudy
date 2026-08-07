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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	appsv1alpha1 "github.com/kylan11/understudy/api/v1alpha1"
	"github.com/kylan11/understudy/internal/metrics"
	"github.com/kylan11/understudy/internal/signal"
)

const (
	AnnotationBaseReplicas    = "understudy.sh/base-replicas"
	AnnotationRelaxed         = "understudy.sh/relaxed"
	AnnotationHeartbeat       = "understudy.sh/heartbeat"
	LabelOwned                = "understudy.sh/owned"
	PodDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
	doomedPodDeletionCost     = "-1000"

	ownedLabelValue     = "true"
	relaxReasonTTL      = "ttl"
	relaxReasonDeadline = "deadline"
	relaxReasonMode     = "mode"

	activeRequeue    = 5 * time.Second
	heartbeatPeriod  = 60 * time.Second
	protectedRequeue = 60 * time.Second
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
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
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

	if kind := cr.Spec.TargetRef.Kind; kind != "" && kind != "Deployment" {
		return r.markUnsupported(ctx, &cr, status,
			fmt.Sprintf("target kind %q is not supported (only Deployment); skipping", kind))
	}
	if cr.Namespace == r.OperatorNamespace && cr.Spec.TargetRef.Name == r.OperatorDeployment && r.OperatorDeployment != "" {
		return r.markUnsupported(ctx, &cr, status,
			"refusing to manage the Understudy operator itself (self-exclusion fail-safe)")
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			setCond(&status, &cr, appsv1alpha1.ConditionTargetSupported, metav1.ConditionFalse,
				"TargetNotFound", fmt.Sprintf("Deployment %q not found", cr.Spec.TargetRef.Name))
			status.Phase = appsv1alpha1.PhaseIdle
			return ctrl.Result{}, r.patchStatus(ctx, &cr, status)
		}
		return ctrl.Result{}, err
	}
	setCond(&status, &cr, appsv1alpha1.ConditionTargetSupported, metav1.ConditionTrue, "Deployment", "target resolved")

	surgeActive := false
	base := int32(1)
	if dep.Spec.Replicas != nil {
		base = *dep.Spec.Replicas
	}
	if v, ok := dep.Annotations[AnnotationBaseReplicas]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil {
			base = int32(parsed)
			surgeActive = true
		}
	}

	r.setStrategyCondition(&status, &cr, &dep, base)

	assessment, err := r.assessPods(ctx, &cr, &dep)
	if err != nil {
		return ctrl.Result{}, err
	}
	surgeReady := assessment.viable >= int(base)

	mode := cr.HostageModeOrDefault()
	relaxReason, ttlExpired := r.relaxDecision(&cr, mode, assessment, surgeReady, status.BlockedSince)
	holdMode := cr.ExperimentalEvictionHold()
	pdbMode := mode
	if holdMode {
		pdbMode = appsv1alpha1.HostageOff
	}
	protected, err := r.ensurePDB(ctx, &cr, &dep, base, pdbMode, relaxReason, len(assessment.doomed) > 0)
	if err != nil {
		return ctrl.Result{}, err
	}
	if holdMode && mode != appsv1alpha1.HostageOff {
		setCond(&status, &cr, appsv1alpha1.ConditionProtected, boolToStatus(relaxReason == ""), "EvictionHold",
			"experimental: evictions are held at admission while a stand-in is prepared; no budget is owned")
	} else {
		setProtectedCondition(&status, &cr, protected, mode, relaxReason)
	}

	switch {
	case len(assessment.doomed) == 0 && !surgeActive:
		status.Phase = appsv1alpha1.PhaseIdle
		status.BlockedSince = nil
		metrics.UntrackBlocked(req.String())
		setCond(&status, &cr, appsv1alpha1.ConditionSurgeReady, boolToStatus(surgeReady), "Steady", "no disruption in sight")
		result := ctrl.Result{}
		if mode != appsv1alpha1.HostageOff {
			result.RequeueAfter = protectedRequeue
		}
		return result, r.patchStatus(ctx, &cr, status)

	case len(assessment.doomed) == 0 && surgeActive:
		return r.reconcileScaleBack(ctx, &cr, &dep, status, assessment, base, req)

	default:
		return r.reconcileDoomed(ctx, &cr, &dep, status, assessment, base, surgeActive, surgeReady, relaxReason, ttlExpired, req)
	}
}

func (r *UnderstudyReconciler) assessPods(ctx context.Context, cr *appsv1alpha1.Understudy, dep *appsv1.Deployment) (podAssessment, error) {
	var a podAssessment
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return a, err
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return a, err
	}
	a.all = podList.Items
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
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
		if podReady(pod) {
			a.viable++
		}
	}
	return a, nil
}

func (r *UnderstudyReconciler) relaxDecision(cr *appsv1alpha1.Understudy, mode appsv1alpha1.HostageMode,
	a podAssessment, surgeReady bool, blockedSince *metav1.Time) (string, bool) {
	relaxReason := ""
	if a.worstDoom != nil && a.worstDoom.Class == signal.Involuntary {
		if mode == appsv1alpha1.HostageVoluntaryOnly {
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
	dep *appsv1.Deployment, status appsv1alpha1.UnderstudyStatus, a podAssessment, base int32,
	req ctrl.Request) (ctrl.Result, error) {
	if err := r.clearDeletionCosts(ctx, a.all); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.scaleTo(ctx, dep, base); err != nil {
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	status.LastReleaseTime = &now
	status.BlockedSince = nil
	status.Phase = appsv1alpha1.PhaseIdle
	metrics.UntrackBlocked(req.String())
	r.event(cr, corev1.EventTypeNormal, "ScaledBack", "Release",
		fmt.Sprintf("predicament over; scaled %s back to %d replicas", dep.Name, base))
	logf.FromContext(ctx).Info("scaled back", "deployment", dep.Name, "replicas", base)
	return ctrl.Result{}, r.patchStatus(ctx, cr, status)
}

func (r *UnderstudyReconciler) reconcileDoomed(ctx context.Context, cr *appsv1alpha1.Understudy,
	dep *appsv1.Deployment, status appsv1alpha1.UnderstudyStatus, a podAssessment, base int32,
	surgeActive, surgeReady bool, relaxReason string, ttlExpired bool, req ctrl.Request) (ctrl.Result, error) {

	if status.BlockedSince == nil {
		now := metav1.Now()
		status.BlockedSince = &now
	}
	metrics.TrackBlocked(req.String(), status.BlockedSince.Time)

	if !surgeActive && rolloutInProgress(dep, base) {
		status.Phase = appsv1alpha1.PhaseIdle
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"RolloutInProgress", "target is mid-rollout; standing down (the Deployment is already surging)")
		r.eventOnce(cr, &status, corev1.EventTypeNormal, "RolloutStandDown", "Skip",
			fmt.Sprintf("doom signal for %s ignored: rollout in progress", dep.Name))
		return ctrl.Result{RequeueAfter: activeRequeue}, r.patchStatus(ctx, cr, status)
	}

	if !surgeActive {
		if err := r.startSurge(ctx, cr, dep, base); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		status.LastSurgeTime = &now
		class := signal.VoluntaryUnbounded
		if a.worstDoom != nil {
			class = a.worstDoom.Class
		}
		metrics.SurgesTotal.WithLabelValues(string(class)).Inc()
		r.event(cr, corev1.EventTypeNormal, "SurgeStarted", "Surge",
			fmt.Sprintf("node doom (%s) for pod(s) of %s; surging %d -> %d",
				doomSource(a.worstDoom), dep.Name, base, base+cr.SurgeReplicasOrDefault()))
	}

	if err := r.stampDeletionCosts(ctx, a.doomed); err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case surgeReady:
		status.Phase = appsv1alpha1.PhaseReleasing
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionTrue,
			"UnderstudyReady", "understudy is traffic-ready; eviction may proceed")
		r.eventOnce(cr, &status, corev1.EventTypeNormal, "SurgeReady", "Release",
			fmt.Sprintf("understudy for %s is traffic-ready; releasing the eviction", dep.Name))
	case relaxReason != "" || ttlExpired:
		status.Phase = appsv1alpha1.PhaseRelaxed
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"Relaxed", "eviction released without a ready understudy ("+relaxReason+")")
		if status.LastReleaseTime == nil || status.LastSurgeTime != nil && status.LastReleaseTime.Before(status.LastSurgeTime) {
			now := metav1.Now()
			status.LastReleaseTime = &now
		}
		r.eventOnce(cr, &status, corev1.EventTypeWarning, "PDBRelaxed", "Relax",
			fmt.Sprintf("hostage PDB for %s relaxed (%s); eviction can proceed without a ready understudy", dep.Name, relaxReason))
	default:
		status.Phase = appsv1alpha1.PhaseSurging
		setCond(&status, cr, appsv1alpha1.ConditionSurgeReady, metav1.ConditionFalse,
			"AwaitingReadiness", fmt.Sprintf("waiting for %d ready replica(s) on healthy nodes (have %d)", base, a.viable))
	}
	return ctrl.Result{RequeueAfter: activeRequeue}, r.patchStatus(ctx, cr, status)
}

func (r *UnderstudyReconciler) startSurge(ctx context.Context, cr *appsv1alpha1.Understudy, dep *appsv1.Deployment, base int32) error {
	patched := dep.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[AnnotationBaseReplicas] = strconv.FormatInt(int64(base), 10)
	target := base + cr.SurgeReplicasOrDefault()
	patched.Spec.Replicas = &target
	return r.Patch(ctx, patched, client.MergeFrom(dep))
}

func (r *UnderstudyReconciler) scaleTo(ctx context.Context, dep *appsv1.Deployment, replicas int32) error {
	patched := dep.DeepCopy()
	patched.Spec.Replicas = &replicas
	delete(patched.Annotations, AnnotationBaseReplicas)
	return r.Patch(ctx, patched, client.MergeFrom(dep))
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

func (r *UnderstudyReconciler) ensurePDB(ctx context.Context, cr *appsv1alpha1.Understudy, dep *appsv1.Deployment,
	base int32, mode appsv1alpha1.HostageMode, relaxReason string, predicamentActive bool) (bool, error) {

	name := "understudy-" + cr.Name
	key := types.NamespacedName{Namespace: cr.Namespace, Name: name}
	var pdb policyv1.PodDisruptionBudget
	getErr := r.Get(ctx, key, &pdb)

	if mode == appsv1alpha1.HostageOff {
		if getErr == nil {
			return false, r.Delete(ctx, &pdb)
		}
		if apierrors.IsNotFound(getErr) {
			return false, nil
		}
		return false, getErr
	}

	desiredMin := intstr.FromInt32(base)
	relaxed := relaxReason != ""
	if relaxed {
		desiredMin = intstr.FromInt32(0)
	}

	if apierrors.IsNotFound(getErr) {
		pdb = policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cr.Namespace,
				Labels: map[string]string{
					LabelOwned:                     ownedLabelValue,
					"app.kubernetes.io/managed-by": "understudy",
				},
				Annotations: map[string]string{
					AnnotationHeartbeat: strconv.FormatInt(time.Now().Unix(), 10),
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &desiredMin,
				Selector:     dep.Spec.Selector,
			},
		}
		if relaxed {
			pdb.Annotations[AnnotationRelaxed] = relaxReason
		}
		if err := ctrl.SetControllerReference(cr, &pdb, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, &pdb); err != nil {
			return false, err
		}
		return !relaxed, nil
	}
	if getErr != nil {
		return false, getErr
	}

	if prev, wasRelaxed := pdb.Annotations[AnnotationRelaxed]; wasRelaxed && predicamentActive && !relaxed {
		relaxed = true
		relaxReason = prev
		desiredMin = intstr.FromInt32(0)
	}

	patched := pdb.DeepCopy()
	patched.Spec.MinAvailable = &desiredMin
	patched.Spec.Selector = dep.Spec.Selector
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	now := time.Now()
	if prev, err := strconv.ParseInt(patched.Annotations[AnnotationHeartbeat], 10, 64); err != nil ||
		now.Sub(time.Unix(prev, 0)) >= heartbeatPeriod {
		patched.Annotations[AnnotationHeartbeat] = strconv.FormatInt(now.Unix(), 10)
	}
	if relaxed {
		if _, ok := patched.Annotations[AnnotationRelaxed]; !ok {
			patched.Annotations[AnnotationRelaxed] = relaxReason
			metrics.RelaxedTotal.WithLabelValues(relaxReason).Inc()
		}
	} else {
		delete(patched.Annotations, AnnotationRelaxed)
	}
	if !equalPDB(&pdb, patched) {
		if err := r.Patch(ctx, patched, client.MergeFrom(&pdb)); err != nil {
			return false, err
		}
	}
	return !relaxed, nil
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

func (r *UnderstudyReconciler) setStrategyCondition(status *appsv1alpha1.UnderstudyStatus,
	cr *appsv1alpha1.Understudy, dep *appsv1.Deployment, base int32) {
	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		setCond(status, cr, appsv1alpha1.ConditionZeroDowntimeStrategy, metav1.ConditionFalse,
			"Recreate", "strategy Recreate drops all traffic on every deploy")
		return
	}
	maxUnavailable, maxSurge := 0, 1
	if ru := dep.Spec.Strategy.RollingUpdate; ru != nil {
		if ru.MaxUnavailable != nil {
			maxUnavailable, _ = intstr.GetScaledValueFromIntOrPercent(ru.MaxUnavailable, int(base), false)
		}
		if ru.MaxSurge != nil {
			maxSurge, _ = intstr.GetScaledValueFromIntOrPercent(ru.MaxSurge, int(base), true)
		}
	}
	if maxUnavailable > 0 || maxSurge < 1 {
		setCond(status, cr, appsv1alpha1.ConditionZeroDowntimeStrategy, metav1.ConditionFalse,
			"LossyStrategy",
			fmt.Sprintf("resolved maxUnavailable=%d, maxSurge=%d: deploys can drop traffic; set maxUnavailable: 0, maxSurge: 1",
				maxUnavailable, maxSurge))
		return
	}
	setCond(status, cr, appsv1alpha1.ConditionZeroDowntimeStrategy, metav1.ConditionTrue, "SurgingStrategy",
		"rolling updates keep capacity during deploys")
}

func setProtectedCondition(status *appsv1alpha1.UnderstudyStatus, cr *appsv1alpha1.Understudy,
	protected bool, mode appsv1alpha1.HostageMode, relaxReason string) {
	if protected {
		setCond(status, cr, appsv1alpha1.ConditionProtected, metav1.ConditionTrue, "HostagePDB", "blocking PDB in place")
		return
	}
	reason, msg := "HostageOff", "hostageMode is off; evictions are not blocked"
	if mode != appsv1alpha1.HostageOff {
		reason, msg = "Relaxed", "hostage PDB relaxed ("+relaxReason+"); evictions may proceed"
	}
	setCond(status, cr, appsv1alpha1.ConditionProtected, metav1.ConditionFalse, reason, msg)
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
		return r.requestsForTarget(ctx, obj.GetNamespace(), obj.GetName())
	}
	mapPod := func(ctx context.Context, obj client.Object) []ctrl.Request {
		return r.requestsInNamespace(ctx, obj.GetNamespace())
	}
	mapDoom := func(ctx context.Context, _ client.Object) []ctrl.Request {
		return r.requestsInNamespace(ctx, "")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Understudy{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(mapDeployment)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPod)).
		WatchesRawSource(source.Channel(r.Registry.Subscribe(64), handler.EnqueueRequestsFromMapFunc(mapDoom))).
		Named("understudy").
		Complete(r)
}

func (r *UnderstudyReconciler) requestsForTarget(ctx context.Context, namespace, name string) []ctrl.Request {
	var list appsv1alpha1.UnderstudyList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		if list.Items[i].Spec.TargetRef.Name == name {
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

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func rolloutInProgress(dep *appsv1.Deployment, base int32) bool {
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionTrue &&
			c.Reason != "NewReplicaSetAvailable" {
			return true
		}
	}
	return dep.Status.UpdatedReplicas < base
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
	if a.Phase != b.Phase || !timePtrEqual(a.LastSurgeTime, b.LastSurgeTime) ||
		!timePtrEqual(a.LastReleaseTime, b.LastReleaseTime) || !timePtrEqual(a.BlockedSince, b.BlockedSince) ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ca, cb := a.Conditions[i], b.Conditions[i]
		if ca.Type != cb.Type || ca.Status != cb.Status || ca.Reason != cb.Reason || ca.Message != cb.Message {
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

func equalPDB(a, b *policyv1.PodDisruptionBudget) bool {
	switch {
	case a.Spec.MinAvailable == nil || b.Spec.MinAvailable == nil:
		if a.Spec.MinAvailable != b.Spec.MinAvailable {
			return false
		}
	case *a.Spec.MinAvailable != *b.Spec.MinAvailable:
		return false
	}
	return a.Annotations[AnnotationRelaxed] == b.Annotations[AnnotationRelaxed] &&
		a.Annotations[AnnotationHeartbeat] == b.Annotations[AnnotationHeartbeat]
}

func doomSource(d *signal.NodeDoom) string {
	if d == nil {
		return "unknown"
	}
	return d.Source
}
