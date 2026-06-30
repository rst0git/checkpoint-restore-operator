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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("PodRestoreReconciler", func() {
	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	})

	const (
		name = "redis-restore"
		ns   = "default"
		tar  = "/var/lib/kubelet/checkpoints/checkpoint-redis_default-redis-x.tar"
	)

	newPodRestore := func() *criuorgv1.PodRestore {
		return &criuorgv1.PodRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: criuorgv1.PodRestoreSpec{
				TargetNode:  "worker-1",
				Checkpoints: []criuorgv1.ContainerCheckpoint{{Container: "redis", Path: tar}},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							// Explicit image so the reconciler need not read the archive.
							{Name: "redis", Image: "redis:7.0"},
						},
					},
				},
			},
		}
	}

	makeReconciler := func(objs ...client.Object) *PodRestoreReconciler {
		c := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(objs...).
			WithStatusSubresource(&criuorgv1.PodRestore{}).
			Build()
		return &PodRestoreReconciler{Client: c, Scheme: scheme.Scheme}
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

	// reconcileN runs Reconcile n times so the resource walks its phase machine
	// (each status write re-enqueues in production; here we drive it explicitly).
	reconcileN := func(r *PodRestoreReconciler, n int) {
		for i := 0; i < n; i++ {
			_, err := r.Reconcile(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	get := func(r *PodRestoreReconciler) *criuorgv1.PodRestore {
		out := &criuorgv1.PodRestore{}
		Expect(r.Get(context.Background(), req.NamespacedName, out)).To(Succeed())
		return out
	}

	It("adds a finalizer and moves to Pending", func() {
		r := makeReconciler(newPodRestore())
		reconcileN(r, 2) // 1: finalizer, 2: phase
		pr := get(r)
		Expect(pr.Finalizers).To(ContainElement(podRestoreFinalizer))
		Expect(pr.Status.Phase).To(Equal(criuorgv1.RestorePhasePending))
	})

	It("renders a node-pinned, annotated Pod and moves to Restoring", func() {
		r := makeReconciler(newPodRestore())
		reconcileN(r, 3) // finalizer -> Pending -> create Pod

		pr := get(r)
		Expect(pr.Status.Phase).To(Equal(criuorgv1.RestorePhaseRestoring))
		Expect(pr.Status.PodName).To(Equal(name))

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		Expect(pod.Spec.NodeName).To(Equal("worker-1"))
		Expect(pod.Spec.Containers[0].Image).To(Equal("redis:7.0"))
		Expect(pod.Annotations).To(HaveKeyWithValue(
			criuorgv1.RestoreCheckpointPathAnnotationPrefix+"redis", tar))
		Expect(pod.Labels).To(HaveKeyWithValue(podRestoreLabel, name))
		Expect(pod.OwnerReferences).To(HaveLen(1))
		Expect(pod.OwnerReferences[0].Name).To(Equal(name))
	})

	It("becomes Running when the Pod is running", func() {
		r := makeReconciler(newPodRestore())
		reconcileN(r, 3)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		Expect(r.Status().Update(context.Background(), pod)).To(Succeed())

		reconcileN(r, 1)
		Expect(get(r).Status.Phase).To(Equal(criuorgv1.RestorePhaseRunning))
	})

	It("fails when a checkpoint references an unknown container", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Container = "does-not-exist"
		r := makeReconciler(pr)
		reconcileN(r, 3) // finalizer -> Pending -> render fails

		out := get(r)
		Expect(out.Status.Phase).To(Equal(criuorgv1.RestorePhaseFailed))
		Expect(out.Status.Message).To(ContainSubstring("not defined in the Pod template"))

		pod := &corev1.Pod{}
		err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("removes the finalizer on deletion", func() {
		r := makeReconciler(newPodRestore())
		reconcileN(r, 3)

		pr := get(r)
		Expect(r.Delete(context.Background(), pr)).To(Succeed())
		reconcileN(r, 1)

		out := &criuorgv1.PodRestore{}
		err := r.Get(context.Background(), req.NamespacedName, out)
		// With the finalizer gone, the fake client purges the object.
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
