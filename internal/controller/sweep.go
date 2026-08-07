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

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type PDBSweep struct {
	Reader client.Reader
	Client client.Client
}

func (s *PDBSweep) NeedLeaderElection() bool {
	return true
}

func (s *PDBSweep) Start(ctx context.Context) error {
	return sweepOwnedPDBs(ctx, s.Reader, s.Client)
}

func sweepOwnedPDBs(ctx context.Context, reader client.Reader, c client.Client) error {
	var pdbs policyv1.PodDisruptionBudgetList
	if err := reader.List(ctx, &pdbs, client.MatchingLabels{LabelOwned: "true"}); err != nil {
		return err
	}
	log := logf.FromContext(ctx)
	for i := range pdbs.Items {
		pdb := &pdbs.Items[i]
		if err := c.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("Deleted legacy PodDisruptionBudget", "namespace", pdb.Namespace, "name", pdb.Name)
	}
	return nil
}
