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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RestoreCheckpointPathAnnotationPrefix is the per-container annotation prefix the
// node-side restore mechanism (CRI proxy or OCI-runtime wrapper) reads to learn
// the checkpoint archive a given container should be restored from. The full key
// is this prefix followed by the container name, e.g.
// "restore.criu.org/checkpoint-path.web". A container without such an annotation
// is created normally.
const RestoreCheckpointPathAnnotationPrefix = "restore.criu.org/checkpoint-path."

// ContainerCheckpoint maps a container in the Pod template to the checkpoint
// archive it should be restored from.
type ContainerCheckpoint struct {
	// Container is the name of the container in the template to restore.
	Container string `json:"container"`
	// Path is the absolute path to the checkpoint .tar archive on the target node.
	Path string `json:"path"`
}

// PodRestoreSpec defines the desired state of PodRestore.
type PodRestoreSpec struct {
	// TargetNode is the node that holds the checkpoint archives. The restored Pod
	// is pinned to this node because the archives are node-local.
	TargetNode string `json:"targetNode"`

	// Checkpoints maps each container to restore to its on-node checkpoint archive.
	// Containers in the template that are not listed here are started normally.
	// +kubebuilder:validation:MinItems=1
	Checkpoints []ContainerCheckpoint `json:"checkpoints"`

	// Template is the PodTemplateSpec describing the restored workload. The
	// controller injects the target node, the per-container restore annotation,
	// and, for restored containers whose image is left empty, the base image
	// recorded in the checkpoint (needed only to satisfy the kubelet image-pull
	// gate; it plays no role in the actual restore).
	Template corev1.PodTemplateSpec `json:"template"`
}

// RestorePhase is the high-level state of a restore.
type RestorePhase string

const (
	// RestorePhasePending means the PodRestore has been accepted and the source
	// checkpoints pinned, but the Pod has not been created yet.
	RestorePhasePending RestorePhase = "Pending"
	// RestorePhaseRestoring means the Pod has been created and is being restored.
	RestorePhaseRestoring RestorePhase = "Restoring"
	// RestorePhaseRunning means the restored Pod is running.
	RestorePhaseRunning RestorePhase = "Running"
	// RestorePhaseFailed means the restore did not succeed.
	RestorePhaseFailed RestorePhase = "Failed"
)

// PodRestoreStatus defines the observed state of PodRestore.
type PodRestoreStatus struct {
	// Phase is the current high-level state of the restore.
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`
	// PodName is the name of the Pod created for this restore.
	// +optional
	PodName string `json:"podName,omitempty"`
	// Message holds the most recent human-readable status or error.
	// +optional
	Message string `json:"message,omitempty"`
	// Conditions represent the latest observations of the restore state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.targetNode`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodRestore is the Schema for the podrestores API.
type PodRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodRestoreSpec   `json:"spec,omitempty"`
	Status PodRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodRestoreList contains a list of PodRestore.
type PodRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodRestore{}, &PodRestoreList{})
}
