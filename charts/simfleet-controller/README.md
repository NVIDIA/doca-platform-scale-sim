# simfleet-controller

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.1.0](https://img.shields.io/badge/AppVersion-v0.1.0-informational?style=flat-square)

SimFleet controller for simulated DPU fleets

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | affinity of the controller Pod. |
| controllerManager | object | `{"containerSecurityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true},"image":{"repository":"ghcr.io/nvidia/doca-platform-scale-sim/simfleet-controller","tag":""},"podSecurityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},"pullPolicy":"IfNotPresent","replicas":1,"resources":{"limits":{"cpu":"500m","memory":"128Mi"},"requests":{"cpu":"10m","memory":"64Mi"}},"serviceAccount":{"annotations":{}},"simFleetMaxConcurrentReconciles":1,"simInstanceMaxConcurrentReconciles":25}` | controllerManager configures the SimFleet controller Deployment. |
| controllerManager.containerSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | containerSecurityContext sets the securityContext applied to the controller container. |
| controllerManager.image.repository | string | `"ghcr.io/nvidia/doca-platform-scale-sim/simfleet-controller"` | image.repository is the controller container image, built with `make docker-build`. |
| controllerManager.image.tag | string | `""` | image.tag is the controller image tag. Defaults to the chart appVersion when empty. |
| controllerManager.podSecurityContext | object | `{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | podSecurityContext sets the pod-level securityContext for the controller Pod. |
| controllerManager.pullPolicy | string | `"IfNotPresent"` | pullPolicy is the imagePullPolicy used for the controller container. |
| controllerManager.replicas | int | `1` | replicas is the number of controller Pods to run. Only the leader reconciles. |
| controllerManager.resources | object | `{"limits":{"cpu":"500m","memory":"128Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}` | resources are the container resources of the controller. |
| controllerManager.serviceAccount | object | `{"annotations":{}}` | serviceAccount configures the controller ServiceAccount. |
| controllerManager.serviceAccount.annotations | object | `{}` | annotations added to the controller ServiceAccount. |
| controllerManager.simFleetMaxConcurrentReconciles | int | `1` | simFleetMaxConcurrentReconciles is how many SimFleets are reconciled at once. |
| controllerManager.simInstanceMaxConcurrentReconciles | int | `25` | simInstanceMaxConcurrentReconciles is how many SimInstances are reconciled at once. One SimInstance is one DPU ordinal. |
| imagePullSecrets | list | `[]` | imagePullSecrets is a list of Secret references used to pull the controller image from private registries. |
| nodeSelector | object | `{}` | nodeSelector places the controller Pod. |
| tolerations | list | `[]` | tolerations of the controller Pod. |

