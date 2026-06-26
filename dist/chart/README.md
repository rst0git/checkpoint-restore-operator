# checkpoint-restore-operator Helm chart

A Helm chart that installs the
[checkpoint-restore-operator](https://github.com/checkpoint-restore/checkpoint-restore-operator):
a Kubernetes operator that manages container checkpoint archives and automates
forensic checkpointing using CRIU.

## Prerequisites

- Kubernetes 1.25+ (forensic container checkpointing must be available on the
  cluster).
- Helm 3.8+ (for OCI registry support).
- The kubelet checkpoint directory (`/var/lib/kubelet/checkpoints` by default)
  must exist on the nodes where the operator runs; the operator mounts it via a
  `hostPath` volume and runs as root to read and prune checkpoint archives.

## Install

### From the OCI registry (ghcr.io)

```sh
helm install checkpoint-restore-operator \
  oci://ghcr.io/checkpoint-restore/charts/checkpoint-restore-operator \
  --namespace checkpoint-restore-operator-system --create-namespace
```

### From the Helm repository (GitHub Pages)

```sh
helm repo add checkpoint-restore \
  https://checkpoint-restore.github.io/checkpoint-restore-operator
helm repo update
helm install checkpoint-restore-operator checkpoint-restore/checkpoint-restore-operator \
  --namespace checkpoint-restore-operator-system --create-namespace
```

## Uninstall

```sh
helm uninstall checkpoint-restore-operator --namespace checkpoint-restore-operator-system
```

> **CRD retention.** By default (`crd.keep=true`) the CustomResourceDefinitions
> are left in the cluster after `helm uninstall`. Deleting them removes every
> `CheckpointRestoreOperator`, `CheckpointSchedule` and `ForensicSnapshotChain`
> custom resource in the cluster, so do it deliberately:
>
> ```sh
> kubectl delete crd \
>   checkpointrestoreoperators.criu.org \
>   checkpointschedules.criu.org \
>   forensicsnapshotchains.criu.org
> ```

## Configuration

Override defaults with `--set key=value` or a custom `-f values.yaml`. The most
commonly changed values:

| Key | Default | Description |
| --- | --- | --- |
| `manager.image.repository` | `quay.io/criu/checkpoint-restore-operator` | Operator image (also on Docker Hub as `criu/checkpoint-restore-operator`). |
| `manager.image.tag` | `latest` | Image tag. Set to `""` to track `Chart.appVersion` once versioned images are published. |
| `manager.replicas` | `1` | Number of controller-manager replicas. |
| `manager.resources` | see values.yaml | CPU/memory requests and limits. |
| `manager.extraVolumes` / `extraVolumeMounts` | kubelet checkpoint hostPath | Mounts kubelet's checkpoint directory; adjust the `hostPath` if your kubelet uses a non-default location. |
| `manager.nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Pod scheduling controls. |
| `manager.securityContext` | runAsUser 0, caps `DAC_OVERRIDE`/`FOWNER` | Container security context; elevated access is required to manage checkpoint archives. |
| `rbac.namespaced` | `false` | Use namespaced Role/RoleBinding instead of cluster-scoped. |
| `serviceAccount.enable` | `true` | Create the operator ServiceAccount. |
| `crd.enable` | `true` | Install the CRDs with the chart. |
| `crd.keep` | `true` | Keep CRDs on `helm uninstall`. |
| `metrics.enable` | `true` | Expose the `/metrics` endpoint. |
| `metrics.secure` | `true` | Serve metrics over HTTPS with authn/authz. |
| `prometheus.enable` | `false` | Create a Prometheus `ServiceMonitor`. |

See [`values.yaml`](./values.yaml) for the complete, commented list.

## Maintaining the chart

This chart is generated from the project's `config/` kustomize bases with the
kubebuilder `helm/v2-alpha` plugin, so it stays in sync with the operator
manifests. After changing anything under `config/`, regenerate with:

```sh
make manifests generate
kubebuilder edit --plugins=helm/v2-alpha --force
```

`--force` regenerates the templates and `values.yaml` (but not `Chart.yaml`).
Re-apply any manual chart edits afterwards and re-run `helm lint dist/chart`.
