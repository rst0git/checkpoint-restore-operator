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
	"fmt"
	"os"
	"path/filepath"
	"time"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

const (
	podRestoreFinalizer = "criu.org/pod-restore-finalizer"
	// podRestoreLabel links a restored Pod back to its PodRestore.
	podRestoreLabel = "restore.criu.org/pod-restore"
)

// PodRestoreReconciler reconciles a PodRestore object into an ordinary, node-pinned
// Pod annotated for the node-side restore mechanism. It is intentionally agnostic
// to how the node bridges "local .tar -> container image" (CRI proxy, OCI-runtime
// wrapper, or local OCI import): it only renders the Pod and tracks its state.
type PodRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=criu.org,resources=podrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=podrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

// Reconcile drives a PodRestore through "" -> Pending -> Restoring -> Running/Failed.
func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pr := &criuorgv1.PodRestore{}
	if err := r.Get(ctx, req.NamespacedName, pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: unpin the source checkpoints and drop the finalizer. The
	// restored Pod is garbage-collected via its owner reference.
	if !pr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pr, podRestoreFinalizer) {
			for _, cp := range pr.Spec.Checkpoints {
				unpinCheckpoint(logger, cp.Path)
			}
			controllerutil.RemoveFinalizer(pr, podRestoreFinalizer)
			if err := r.Update(ctx, pr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(pr, podRestoreFinalizer) {
		controllerutil.AddFinalizer(pr, podRestoreFinalizer)
		if err := r.Update(ctx, pr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	switch pr.Status.Phase {
	case criuorgv1.RestorePhaseRunning, criuorgv1.RestorePhaseFailed:
		// Terminal.
		return ctrl.Result{}, nil

	case "":
		// Pin the source checkpoints so retention does not delete them while the
		// restore is in flight, then move to Pending.
		for _, cp := range pr.Spec.Checkpoints {
			pinCheckpoint(logger, cp.Path)
		}
		return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
			s.Phase = criuorgv1.RestorePhasePending
			setReady(s, metav1.ConditionFalse, "Pending", "Pinned source checkpoints; preparing restore Pod")
		})

	case criuorgv1.RestorePhasePending:
		pod, err := r.renderPod(logger, pr)
		if err != nil {
			// A bad spec (unknown container, unresolved image) is not retryable.
			logger.Error(err, "failed to render restore Pod")
			return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
				s.Phase = criuorgv1.RestorePhaseFailed
				s.Message = err.Error()
				setReady(s, metav1.ConditionFalse, "RenderFailed", err.Error())
			})
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
			s.Phase = criuorgv1.RestorePhaseRestoring
			s.PodName = pod.Name
			setReady(s, metav1.ConditionFalse, "Restoring", "Restore Pod created")
		})

	case criuorgv1.RestorePhaseRestoring:
		pod := &corev1.Pod{}
		key := client.ObjectKey{Namespace: pr.Namespace, Name: pr.Status.PodName}
		if err := r.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
					s.Phase = criuorgv1.RestorePhaseFailed
					s.Message = "restore Pod disappeared"
					setReady(s, metav1.ConditionFalse, "PodMissing", "restore Pod disappeared")
				})
			}
			return ctrl.Result{}, err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded:
			return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
				s.Phase = criuorgv1.RestorePhaseRunning
				s.Message = "restored Pod is running"
				setReady(s, metav1.ConditionTrue, "Restored", "Restored Pod is running")
			})
		case corev1.PodFailed:
			return r.updateStatus(ctx, pr, func(s *criuorgv1.PodRestoreStatus) {
				s.Phase = criuorgv1.RestorePhaseFailed
				s.Message = "restore Pod failed: " + pod.Status.Reason
				setReady(s, metav1.ConditionFalse, "PodFailed", pod.Status.Reason)
			})
		default:
			// Still pending/unknown; re-check (the Pod watch also re-enqueues us).
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}

	default:
		logger.Info("unrecognized phase, taking no action", "phase", pr.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// renderPod builds the restore Pod from the template, pinning it to the target
// node, annotating each restored container with its checkpoint path, and filling
// in the base image from the checkpoint when the template leaves it empty.
func (r *PodRestoreReconciler) renderPod(logger logr.Logger, pr *criuorgv1.PodRestore) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: *pr.Spec.Template.ObjectMeta.DeepCopy(),
		Spec:       *pr.Spec.Template.Spec.DeepCopy(),
	}
	pod.Name = pr.Name
	pod.Namespace = pr.Namespace
	pod.Spec.NodeName = pr.Spec.TargetNode

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[podRestoreLabel] = pr.Name

	for _, cp := range pr.Spec.Checkpoints {
		if err := validateCheckpointPath(cp.Path); err != nil {
			return nil, fmt.Errorf("container %q: %w", cp.Container, err)
		}
		idx := containerIndex(pod.Spec.Containers, cp.Container)
		if idx < 0 {
			return nil, fmt.Errorf("container %q is not defined in the Pod template", cp.Container)
		}
		// The node-side restore mechanism reads this to restore the container.
		pod.Annotations[criuorgv1.RestoreCheckpointPathAnnotationPrefix+cp.Container] = cp.Path

		// The image only satisfies the kubelet's image-pull gate. Prefer an
		// explicit template image; otherwise use the checkpoint's base image.
		if pod.Spec.Containers[idx].Image == "" {
			img, err := readCheckpointBaseImage(logger, cp.Path)
			if err != nil {
				return nil, fmt.Errorf("container %q: %w", cp.Container, err)
			}
			pod.Spec.Containers[idx].Image = img
		}
	}

	if err := controllerutil.SetControllerReference(pr, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

// updateStatus mutates and persists the PodRestore status, bumping observedGeneration.
func (r *PodRestoreReconciler) updateStatus(
	ctx context.Context,
	pr *criuorgv1.PodRestore,
	mutate func(*criuorgv1.PodRestoreStatus),
) (ctrl.Result, error) {
	mutate(&pr.Status)
	pr.Status.ObservedGeneration = pr.Generation
	if err := r.Status().Update(ctx, pr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func setReady(s *criuorgv1.PodRestoreStatus, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// validateCheckpointPath rejects paths that are unsafe to hand to the node-side
// restore mechanism: it requires an absolute, lexically-clean path (no "." or
// ".." traversal) ending in .tar. This is defense in depth alongside the CRD's
// schema validation and mirrors the runtime's own checkpoint-archive checks.
func validateCheckpointPath(p string) error {
	if p == "" {
		return fmt.Errorf("checkpoint path is empty")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("checkpoint path %q must be absolute", p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("checkpoint path %q must be clean (no '.', '..', or redundant separators)", p)
	}
	if filepath.Ext(p) != ".tar" {
		return fmt.Errorf("checkpoint path %q must be a .tar archive", p)
	}
	return nil
}

func containerIndex(containers []corev1.Container, name string) int {
	for i := range containers {
		if containers[i].Name == name {
			return i
		}
	}
	return -1
}

// readCheckpointBaseImage extracts the base (rootfs) image name recorded in a
// checkpoint archive's config.dump. This requires the archive to be readable from
// where the controller runs; if it is not, the user should set the container image
// explicitly in the template.
func readCheckpointBaseImage(logger logr.Logger, checkpointPath string) (string, error) {
	tempDir, err := os.MkdirTemp("", "podrestore-image")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	if err := UntarFiles(checkpointPath, tempDir, []string{"config.dump"}); err != nil {
		return "", fmt.Errorf("extracting config.dump from %s: %w (set the container image explicitly if the archive is not reachable from the operator)", checkpointPath, err)
	}
	cfg, _, err := metadata.ReadContainerCheckpointConfigDump(tempDir)
	if err != nil {
		return "", fmt.Errorf("reading checkpoint config.dump: %w", err)
	}
	if cfg.RootfsImageName == "" {
		return "", fmt.Errorf("checkpoint %s records no rootfsImageName; set the container image explicitly", checkpointPath)
	}
	logger.V(1).Info("resolved base image from checkpoint", "path", checkpointPath, "image", cfg.RootfsImageName)
	return cfg.RootfsImageName, nil
}

// pinCheckpoint writes a .keep marker next to the archive so the garbage collector
// retains it. Best-effort: the archive is node-local, so this only takes effect
// when the path is reachable from where the controller runs.
func pinCheckpoint(logger logr.Logger, path string) {
	if _, err := os.Stat(path); err != nil {
		logger.Info("checkpoint not reachable from controller; pinning must be handled node-side", "path", path)
		return
	}
	keep := path + ".keep"
	f, err := os.OpenFile(keep, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logger.Error(err, "failed to write .keep marker", "path", keep)
		return
	}
	_ = f.Close()
}

// unpinCheckpoint removes the .keep marker. Best-effort.
func unpinCheckpoint(logger logr.Logger, path string) {
	if err := os.Remove(path + ".keep"); err != nil && !os.IsNotExist(err) {
		logger.Error(err, "failed to remove .keep marker", "path", path+".keep")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.PodRestore{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
