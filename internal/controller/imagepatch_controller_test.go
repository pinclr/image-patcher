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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
)

var _ = Describe("ImagePatch Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		imagepatch := &omsv1alpha1.ImagePatch{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ImagePatch")
			err := k8sClient.Get(ctx, typeNamespacedName, imagepatch)
			if err != nil && errors.IsNotFound(err) {
				resource := &omsv1alpha1.ImagePatch{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &omsv1alpha1.ImagePatch{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ImagePatch")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ImagePatchReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				KanikoImage: "gcr.io/kaniko-project/executor:v1.23.2",
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	// MSP-117 regression: when the CR's namespace differs from the
	// build namespace, Jobs / ConfigMaps / Secrets cannot carry an
	// OwnerReference back to the CR, so GC will not cascade-delete
	// them. The controller must drive cleanup itself via a finalizer.
	Context("When the CR is deleted in a cross-namespace build", func() {
		const (
			crName = "msp117-cleanup"
			crNs   = "default"
		)
		key := types.NamespacedName{Name: crName, Namespace: crNs}

		reconciler := &ImagePatchReconciler{
			Client:      k8sClient,
			Scheme:      nil, // populated in BeforeEach when k8sClient is ready
			KanikoImage: "gcr.io/kaniko-project/executor:v1.23.2",
		}

		BeforeEach(func() {
			reconciler.Scheme = k8sClient.Scheme()

			cr := &omsv1alpha1.ImagePatch{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: crNs},
				Spec: omsv1alpha1.ImagePatchSpec{
					BaseImage: "ubuntu:22.04",
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() {
			// Best-effort: if the test left a CR (e.g. assertion failed
			// before cleanup), strip the finalizer so envtest can drop
			// it; otherwise subsequent test runs would collide on name.
			cr := &omsv1alpha1.ImagePatch{}
			if err := k8sClient.Get(ctx, key, cr); err == nil {
				cr.Finalizers = nil
				_ = k8sClient.Update(ctx, cr)
				_ = k8sClient.Delete(ctx, cr)
			}
		})

		It("installs a finalizer and cleans up build resources on delete", func() {
			By("first reconcile installs the finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			cr := &omsv1alpha1.ImagePatch{}
			Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
			Expect(cr.Finalizers).To(ContainElement(imagePatchFinalizer))

			By("second reconcile creates the build Job + ConfigMap in image-patch-system")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs,
				client.InNamespace(defaultBuildNamespace),
				client.MatchingLabels{
					"imagepatch.source.name":      crName,
					"imagepatch.source.namespace": crNs,
				},
			)).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1), "expected exactly one build Job stamped with source labels")

			cms := &corev1.ConfigMapList{}
			Expect(k8sClient.List(ctx, cms,
				client.InNamespace(defaultBuildNamespace),
				client.MatchingLabels{
					"imagepatch.source.name":      crName,
					"imagepatch.source.namespace": crNs,
				},
			)).To(Succeed())
			Expect(cms.Items).To(HaveLen(1), "expected the Dockerfile ConfigMap")

			By("deleting the CR sets DeletionTimestamp because the finalizer is in place")
			Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, cr)).To(Succeed())
			Expect(cr.DeletionTimestamp.IsZero()).To(BeFalse())

			By("reconcile after delete tears down build resources and removes the finalizer")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.List(ctx, jobs,
				client.InNamespace(defaultBuildNamespace),
				client.MatchingLabels{
					"imagepatch.source.name":      crName,
					"imagepatch.source.namespace": crNs,
				},
			)).To(Succeed())
			Expect(jobs.Items).To(BeEmpty(), "build Job should be cleaned up by the finalizer")

			Expect(k8sClient.List(ctx, cms,
				client.InNamespace(defaultBuildNamespace),
				client.MatchingLabels{
					"imagepatch.source.name":      crName,
					"imagepatch.source.namespace": crNs,
				},
			)).To(Succeed())
			Expect(cms.Items).To(BeEmpty(), "Dockerfile ConfigMap should be cleaned up by the finalizer")

			err = k8sClient.Get(ctx, key, cr)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "CR should be physically removed once the finalizer is gone")
		})
	})
})
