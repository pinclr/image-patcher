/*
Copyright 2026.

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

package metrics

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
)

// PhaseAggregator is a controller-runtime Runnable that periodically
// lists ImagePatch CRs and publishes counts to image_patcher_imagepatches
// and image_patcher_active_builds. Per-reconcile updates can't supply
// these because they only fire on transitions of the CR currently being
// reconciled; an idle steady state would never refresh the gauges.
//
// List goes through the manager's cache, so this costs effectively
// nothing once the cache is warm.
type PhaseAggregator struct {
	Client   client.Client
	Interval time.Duration
}

// Start runs the aggregator loop until ctx is cancelled. controller-runtime
// invokes Start on each manager Runnable after caches have synced.
func (a *PhaseAggregator) Start(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	a.tick(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

func (a *PhaseAggregator) tick(ctx context.Context) {
	var list omsv1alpha1.ImagePatchList
	if err := a.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).WithName("phase-aggregator").Error(err, "list ImagePatches failed")
		return
	}
	counts := map[string]int{}
	for i := range list.Items {
		phase := list.Items[i].Status.Phase
		if phase == "" {
			phase = PhasePending
		}
		counts[phase]++
	}
	SetImagePatchesByPhase(counts)
}
