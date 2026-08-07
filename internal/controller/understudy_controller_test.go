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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/kylan11/understudy/api/v1alpha1"
	"github.com/kylan11/understudy/internal/signal"
)

var nsCounter int

const appLabel = "app"

type fixture struct {
	ns   string
	node *corev1.Node
	dep  *appsv1.Deployment
	pod  *corev1.Pod
	cr   *appsv1alpha1.Understudy
}

func newFixture(mutateCR func(*appsv1alpha1.Understudy)) *fixture {
	nsCounter++
	f := &fixture{ns: fmt.Sprintf("test-%d-%d", GinkgoRandomSeed(), nsCounter)}

	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: f.ns},
	})).To(Succeed())

	f.node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-node"}}
	Expect(k8sClient.Create(ctx, f.node)).To(Succeed())

	f.dep = makeDeployment(f.ns, "lead", 1)
	Expect(k8sClient.Create(ctx, f.dep)).To(Succeed())
	settleDeployment(f.dep)

	f.pod = makePod(f.ns, "lead-0", "lead", f.node.Name, true)

	f.cr = &appsv1alpha1.Understudy{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: f.ns},
		Spec: appsv1alpha1.UnderstudySpec{
			TargetRef: appsv1alpha1.TargetReference{Name: "lead"},
		},
	}
	if mutateCR != nil {
		mutateCR(f.cr)
	}
	Expect(k8sClient.Create(ctx, f.cr)).To(Succeed())
	return f
}

func makeDeployment(ns, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabel: name}},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(0)),
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "registry.k8s.io/pause:3.10",
					}},
				},
			},
		},
	}
}

func settleDeployment(dep *appsv1.Deployment) {
	Eventually(func() error {
		current := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), current); err != nil {
			return err
		}
		replicas := *current.Spec.Replicas
		current.Status = appsv1.DeploymentStatus{
			ObservedGeneration: current.Generation,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentProgressing,
				Status: corev1.ConditionTrue,
				Reason: "NewReplicaSetAvailable",
			}},
		}
		return k8sClient.Status().Update(ctx, current)
	}).Should(Succeed())
}

func makePod(ns, name, app, node string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{appLabel: app},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "registry.k8s.io/pause:3.10",
			}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	if ready {
		markPodReady(pod)
	}
	return pod
}

func markPodReady(pod *corev1.Pod) {
	Eventually(func() error {
		current := &corev1.Pod{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
			return err
		}
		current.Status.Phase = corev1.PodRunning
		current.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
		return k8sClient.Status().Update(ctx, current)
	}).Should(Succeed())
}

func cordonNode(node *corev1.Node) {
	Eventually(func() error {
		current := &corev1.Node{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
			return err
		}
		current.Spec.Unschedulable = true
		return k8sClient.Update(ctx, current)
	}).Should(Succeed())
}

func taintNode(node *corev1.Node, key string) {
	Eventually(func() error {
		current := &corev1.Node{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), current); err != nil {
			return err
		}
		current.Spec.Taints = append(current.Spec.Taints, corev1.Taint{
			Key:    key,
			Value:  "1754400000",
			Effect: corev1.TaintEffectNoSchedule,
		})
		return k8sClient.Update(ctx, current)
	}).Should(Succeed())
}

func getDeployment(f *fixture) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(f.dep), dep)).To(Succeed())
	return dep
}

func getCR(f *fixture) *appsv1alpha1.Understudy {
	cr := &appsv1alpha1.Understudy{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(f.cr), cr)).To(Succeed())
	return cr
}

func getPDB(f *fixture) error {
	pdb := &policyv1.PodDisruptionBudget{}
	return k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "understudy-" + f.cr.Name}, pdb)
}

func expectSurged(f *fixture) {
	EventuallyWithOffset(1, func() int32 {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(f.dep), dep); err != nil || dep.Spec.Replicas == nil {
			return -1
		}
		return *dep.Spec.Replicas
	}).Should(Equal(int32(2)), "deployment should surge 1 -> 2")
	EventuallyWithOffset(1, func() string {
		dep := &appsv1.Deployment{}
		_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(f.dep), dep)
		return dep.Annotations[AnnotationBaseReplicas]
	}).Should(Equal("1"), "base replicas should be recorded")
	EventuallyWithOffset(1, func() string {
		return getPodDeletionCost(f, f.pod.Name)
	}).Should(Equal(doomedPodDeletionCost), "doomed pod should carry negative deletion cost")
}

var _ = Describe("Understudy controller", func() {

	It("deletes legacy owned budgets and preserves unrelated budgets", func() {
		f := newFixture(nil)
		owned := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "legacy-owned",
				Namespace: f.ns,
				Labels:    map[string]string{LabelOwned: "true"},
			},
		}
		unowned := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "unowned", Namespace: f.ns},
		}
		Expect(k8sClient.Create(ctx, owned)).To(Succeed())
		Expect(k8sClient.Create(ctx, unowned)).To(Succeed())

		Expect(sweepOwnedPDBs(ctx, k8sClient, k8sClient)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(owned), &policyv1.PodDisruptionBudget{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(unowned), &policyv1.PodDisruptionBudget{})).To(Succeed())
	})

	It("owns no objects and stays Idle on a healthy node", func() {
		f := newFixture(nil)

		By("owning no PodDisruptionBudget")
		Consistently(func() bool {
			return apierrors.IsNotFound(getPDB(f))
		}).Should(BeTrue())

		By("reporting Idle and never touching replicas")
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))
		Expect(findCond(getCR(f), appsv1alpha1.ConditionProtected)).To(And(
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", "EvictionHold"),
		))
		Consistently(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Expect(getDeployment(f).Annotations).NotTo(HaveKey(AnnotationBaseReplicas))
	})

	It("surges on node cordon (the universal drain dialect)", func() {
		f := newFixture(nil)
		cordonNode(f.node)
		expectSurged(f)
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseSurging))
		Expect(getCR(f).Status.BlockedSince).NotTo(BeNil())
		Expect(getCR(f).Status.LastSurgeTime).NotTo(BeNil())
	})

	for _, key := range []string{
		"karpenter.sh/disrupted",
		"ToBeDeletedByClusterAutoscaler",
		"DeletionCandidateOfClusterAutoscaler",
	} {
		It("surges on the default drain taint "+key, func() {
			f := newFixture(nil)
			taintNode(f.node, key)
			expectSurged(f)
		})
	}

	It("does not surge on unrelated taints or healthy nodes", func() {
		f := newFixture(nil)
		taintNode(f.node, "example.com/some-unrelated-taint")
		Consistently(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Expect(getCR(f).Status.Phase).To(Equal(appsv1alpha1.PhaseIdle))
	})

	It("stands down while the target is mid-rollout", func() {
		f := newFixture(nil)

		By("making the Deployment look mid-rollout")
		Eventually(func() error {
			dep := getDeployment(f)
			dep.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentProgressing,
				Status: corev1.ConditionTrue,
				Reason: "ReplicaSetUpdated",
			}}
			dep.Status.UpdatedReplicas = 0
			return k8sClient.Status().Update(ctx, dep)
		}).Should(Succeed())

		cordonNode(f.node)

		By("never initiating a surge (the Deployment is already surging)")
		Consistently(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Expect(getDeployment(f).Annotations).NotTo(HaveKey(AnnotationBaseReplicas))
	})

	It("releases and scales back removing the old pod, not the understudy", func() {
		f := newFixture(nil)
		cordonNode(f.node)
		expectSurged(f)

		By("bringing the understudy up on a healthy node")
		healthy := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-node-healthy"}}
		Expect(k8sClient.Create(ctx, healthy)).To(Succeed())
		understudyPod := makePod(f.ns, "lead-understudy", "lead", healthy.Name, true)

		By("entering Releasing once the understudy is traffic-ready")
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseReleasing))

		By("confirming only the doomed pod carries the negative deletion cost")
		Expect(getPodDeletionCost(f, f.pod.Name)).To(Equal(doomedPodDeletionCost))
		Expect(getPodDeletionCost(f, understudyPod.Name)).To(BeEmpty())

		By("simulating the drainer's now-permitted eviction of the old pod")
		Expect(k8sClient.Delete(ctx, f.pod)).To(Succeed())

		By("scaling back to base replicas with the understudy surviving")
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Eventually(func() map[string]string { return getDeployment(f).Annotations }).ShouldNot(HaveKey(AnnotationBaseReplicas))
		Expect(getCR(f).Status.LastReleaseTime).NotTo(BeNil())
		survivor := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(understudyPod), survivor)).To(Succeed())
		Expect(survivor.DeletionTimestamp).To(BeNil())
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))
	})

	It("un-surges cleanly when the doom signal is retracted (uncordon)", func() {
		f := newFixture(nil)
		cordonNode(f.node)
		expectSurged(f)

		By("uncordoning the node")
		Eventually(func() error {
			node := &corev1.Node{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(f.node), node); err != nil {
				return err
			}
			node.Spec.Unschedulable = false
			return k8sClient.Update(ctx, node)
		}).Should(Succeed())

		By("scaling back and clearing the deletion-cost hint from the old pod")
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Eventually(func() string { return getPodDeletionCost(f, f.pod.Name) }).Should(BeEmpty())
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))
	})

	It("fires the never-wedge TTL and admits the eviction", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.ReadinessDeadlineSeconds = ptr.To(int32(1))
		})
		cordonNode(f.node)
		expectSurged(f)

		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseRelaxed))
		Eventually(func() error { return evict(f.pod, false) }).Should(Succeed())
	})

	It("releases immediately when an involuntary deadline is shorter than minSurgeTimeSeconds", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.MinSurgeTimeSeconds = ptr.To(int32(300))
		})

		By("injecting an involuntary doom with a 30s deadline (spot-reclaim shape)")
		deadline := time.Now().Add(30 * time.Second)
		registry.Set(signal.NodeDoom{
			Node:     f.node.Name,
			Class:    signal.Involuntary,
			Deadline: &deadline,
			Source:   "test:injected-itn",
		})
		DeferCleanup(func() { registry.Clear(f.node.Name, "test:injected-itn") })

		By("relaxing the hold at once because waiting cannot help below minSurgeTime")
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseRelaxed))

		By("still surging in parallel to minimize the gap")
		expectSurged(f)

		By("admitting the eviction")
		Eventually(func() error { return evict(f.pod, false) }).Should(Succeed())
	})

	It("skips unsupported target kinds with a condition, never error-looping", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.TargetRef.Kind = "StatefulSet"
		})
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseUnsupported))
		cond := findCond(getCR(f), appsv1alpha1.ConditionTargetSupported)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Consistently(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseUnsupported))
	})

	It("creates no PDB with hostageMode off but still surges on doom", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.HostageMode = appsv1alpha1.HostageOff
		})
		Consistently(func() bool {
			return apierrors.IsNotFound(getPDB(f))
		}).Should(BeTrue(), "no PDB may exist in hostageMode=off")

		cordonNode(f.node)
		expectSurged(f)
	})

	It("refuses to manage the operator itself (self-exclusion)", func() {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: testOperatorNamespace},
		})).To(Succeed())
		cr := &appsv1alpha1.Understudy{
			ObjectMeta: metav1.ObjectMeta{Name: "self", Namespace: testOperatorNamespace},
			Spec: appsv1alpha1.UnderstudySpec{
				TargetRef: appsv1alpha1.TargetReference{Name: testOperatorDeployment},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		Eventually(func() appsv1alpha1.Phase {
			current := &appsv1alpha1.Understudy{}
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), current)
			return current.Status.Phase
		}).Should(Equal(appsv1alpha1.PhaseUnsupported))
	})
})

func getPodDeletionCost(f *fixture, podName string) string {
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: podName}, pod); err != nil {
		return ""
	}
	return pod.Annotations[PodDeletionCostAnnotation]
}

func findCond(cr *appsv1alpha1.Understudy, condType string) *metav1.Condition {
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == condType {
			return &cr.Status.Conditions[i]
		}
	}
	return nil
}
