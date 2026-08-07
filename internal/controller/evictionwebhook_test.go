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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/kylan11/understudy/api/v1alpha1"
	"github.com/kylan11/understudy/internal/signal"
)

func evict(pod *corev1.Pod, dryRun bool) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	var opts []client.SubResourceCreateOption
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	return k8sClient.SubResource("eviction").Create(ctx, pod, eviction, opts...)
}

var _ = Describe("Eviction webhook adapter", func() {

	It("surges when a protected pod's eviction is attempted, with no node signal at all", func() {
		f := newFixture(nil)

		By("attempting an eviction the webhook will reject")
		Eventually(func() bool {
			err := evict(f.pod, false)
			return err != nil
		}).Should(BeTrue(), "the eviction hold should reject the eviction with 429")

		By("surging purely on the observed eviction attempt")
		Eventually(func() int32 {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(f.dep), dep); err != nil {
				return -1
			}
			return *dep.Spec.Replicas
		}).Should(Equal(int32(2)))
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseSurging))
	})

	It("ignores evictions of pods that no Understudy protects", func() {
		f := newFixture(nil)
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))

		By("creating an unrelated pod on the same node")
		other := makePod(f.ns, "bystander", "bystander", f.node.Name, true)

		By("evicting the bystander")
		Expect(evict(other, false)).To(Succeed())

		By("never surging the protected workload")
		Consistently(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Expect(getCR(f).Status.Phase).To(Equal(appsv1alpha1.PhaseIdle))
	})

	It("ignores dry-run eviction attempts", func() {
		f := newFixture(nil)
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))

		By("issuing a dry-run eviction")
		_ = evict(f.pod, true)

		By("never surging")
		Consistently(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
	})

	It("holds evictions at admission instead of owning a budget", func() {
		f := newFixture(nil)

		By("owning no budget at all")
		Consistently(func() bool {
			return apierrors.IsNotFound(getPDB(f))
		}).Should(BeTrue())

		By("answering the eviction with 429, exactly as a budget would")
		var evictErr error
		Eventually(func() bool {
			evictErr = evict(f.pod, false)
			return evictErr != nil
		}).Should(BeTrue())
		Expect(apierrors.IsTooManyRequests(evictErr)).To(BeTrue(),
			fmt.Sprintf("the hold must be indistinguishable from a budget block; got: %v", evictErr))

		By("surging on the held attempt")
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(2)))

		By("admitting the eviction once the stand-in is ready")
		clearNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-clear"}}
		Expect(k8sClient.Create(ctx, clearNode)).To(Succeed())
		makePod(f.ns, "lead-understudy", f.dep.Name, clearNode.Name, true)
		Eventually(func() error { return evict(f.pod, false) }).Should(Succeed())

		By("scaling back once the old pod is gone")
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))
	})

	It("never wedges in hold mode: the readiness TTL admits the eviction", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.ReadinessDeadlineSeconds = ptr.To(int32(3))
		})

		By("holding the first attempts")
		Eventually(func() bool { return evict(f.pod, false) != nil }).Should(BeTrue())
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(2)))
		makePod(f.ns, "lead-understudy", f.dep.Name, f.node.Name, false)

		By("admitting after the deadline even with no ready stand-in")
		Eventually(func() error { return evict(f.pod, false) }, 20*time.Second).Should(Succeed())
	})

	It("un-surges once eviction attempts stop and the signal expires", func() {
		f := newFixture(nil)

		By("triggering a surge via an eviction attempt")
		Eventually(func() bool { return evict(f.pod, false) != nil }).Should(BeTrue())
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(2)))

		By("letting the signal expire naturally once attempts stop")
		Eventually(func() int32 { return *getDeployment(f).Spec.Replicas }).Should(Equal(int32(1)))
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseIdle))
	})

	It("keeps holding when a ready stand-in is on the victim or another doomed node", func() {
		f := newFixture(nil)
		Eventually(func() bool { return evict(f.pod, false) != nil }).Should(BeTrue())
		makePod(f.ns, "same-node-understudy", f.dep.Name, f.node.Name, true)
		Consistently(func() bool { return apierrors.IsTooManyRequests(evict(f.pod, false)) }).Should(BeTrue())

		doomedNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-doomed"}}
		Expect(k8sClient.Create(ctx, doomedNode)).To(Succeed())
		registry.Set(signal.NodeDoom{Node: doomedNode.Name, Class: signal.VoluntaryUnbounded, Source: "test"})
		DeferCleanup(func() { registry.Clear(doomedNode.Name, "test") })
		makePod(f.ns, "doomed-node-understudy", f.dep.Name, doomedNode.Name, true)
		Consistently(func() bool { return apierrors.IsTooManyRequests(evict(f.pod, false)) }).Should(BeTrue())

		clearNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-clear"}}
		Expect(k8sClient.Create(ctx, clearNode)).To(Succeed())
		makePod(f.ns, "clear-node-understudy", f.dep.Name, clearNode.Name, true)
		Eventually(func() error { return evict(f.pod, false) }).Should(Succeed())
	})

	It("never holds an unsupported target", func() {
		f := newFixture(func(cr *appsv1alpha1.Understudy) {
			cr.Spec.TargetRef.Kind = "StatefulSet"
		})
		Eventually(func() appsv1alpha1.Phase { return getCR(f).Status.Phase }).Should(Equal(appsv1alpha1.PhaseUnsupported))
		Expect(evict(f.pod, false)).To(Succeed())
	})

	It("requires every matching protector to admit", func() {
		f := newFixture(nil)
		second := makeDeployment(f.ns, "second", 2)
		second.Spec.Selector = f.dep.Spec.Selector.DeepCopy()
		second.Spec.Template.Labels = map[string]string{appLabel: f.dep.Name}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		settleDeployment(second)
		secondCR := &appsv1alpha1.Understudy{
			ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: f.ns},
			Spec: appsv1alpha1.UnderstudySpec{
				TargetRef: appsv1alpha1.TargetReference{Name: second.Name},
			},
		}
		Expect(k8sClient.Create(ctx, secondCR)).To(Succeed())

		Eventually(func() bool { return evict(f.pod, false) != nil }).Should(BeTrue())
		clearNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.ns + "-clear-multi"}}
		Expect(k8sClient.Create(ctx, clearNode)).To(Succeed())
		makePod(f.ns, "first-clear", f.dep.Name, clearNode.Name, true)
		Consistently(func() bool { return apierrors.IsTooManyRequests(evict(f.pod, false)) }).Should(BeTrue())
		makePod(f.ns, "second-clear", f.dep.Name, clearNode.Name, true)
		Eventually(func() error { return evict(f.pod, false) }).Should(Succeed())
	})
})
