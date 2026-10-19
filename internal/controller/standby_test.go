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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kylan11/understudy/internal/signal"
	"github.com/kylan11/understudy/internal/signal/cordon"
	"github.com/kylan11/understudy/internal/signal/taint"
)

var _ = Describe("A replica without the leader lease", func() {
	It("still sees node doom signals, since it serves the webhook too", func() {
		const leaseName = "standby.understudy.sh"

		By("leaving the lease to another replica")
		now := metav1.NewMicroTime(time.Now())
		Expect(k8sClient.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: metav1.NamespaceDefault},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To("leader"),
				LeaseDurationSeconds: ptr.To(int32(3600)),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		})).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                  scheme.Scheme,
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			LeaderElection:          true,
			LeaderElectionID:        leaseName,
			LeaderElectionNamespace: metav1.NamespaceDefault,
			Controller:              config.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		standby := signal.NewRegistry()
		Expect(cordon.New().SetupWithManager(mgr, standby)).To(Succeed())
		Expect(taint.New(nil).SetupWithManager(mgr, standby)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			// A replica that never led reports "leader election lost" on shutdown.
			_ = mgr.Start(mgrCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(stopped).Should(BeClosed())
		})

		By("cordoning one node and tainting another")
		cordoned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "standby-cordoned"}}
		tainted := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "standby-tainted"}}
		Expect(k8sClient.Create(ctx, cordoned)).To(Succeed())
		Expect(k8sClient.Create(ctx, tainted)).To(Succeed())
		cordonNode(cordoned)
		taintNode(tainted, "ToBeDeletedByClusterAutoscaler")

		By("recording both in the standby's registry without ever leading")
		Eventually(func() bool { _, doomed := standby.Lookup(cordoned.Name); return doomed }).Should(BeTrue())
		Eventually(func() bool { _, doomed := standby.Lookup(tainted.Name); return doomed }).Should(BeTrue())
		Expect(mgr.Elected()).NotTo(BeClosed())
	})
})
