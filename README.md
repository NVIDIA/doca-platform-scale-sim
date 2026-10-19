# DOCA Platform Scale Sim

DOCA Platform Scale Sim runs fleets of simulated BlueField DPUs on Kubernetes,
so the [DOCA Platform Framework](https://github.com/NVIDIA/doca-platform) (DPF)
can be exercised at scale without the hardware. A `SimFleet` asks for a number
of DPUs; the controller backs each one with a KubeVirt VirtualMachine, a mock
Redfish BMC, and the DPF inventory objects (`DPUDevice`, `DPUNode`) that DPF
expects to find for a real device.

**Status:** migration in progress. The API, controller and packaging are here;
end-to-end tests and CI follow in the next pull requests.

## Build and test

Requires Go 1.26.

```bash
make test
make lint
make verify
```

`make test` runs the unit and envtest tests with the race detector.
`make verify` checks that the generated CRDs, RBAC, deepcopy code, `go.mod` and
chart README are up to date, that shell scripts are formatted with `shfmt`,
that sources carry the copyright notice, and runs checkov against the rendered
chart, which needs Docker. Run `make manifests generate` after changing the
API, and `make fmt-shell` to format shell scripts.

## Install

The controller runs on a DPF management cluster with KubeVirt installed.
Build and push the image, then install the CRDs and controller with the Helm
chart. `REGISTRY` defaults to the placeholder `example.com`, so name the
registry explicitly:

```bash
make docker-build docker-push IMG=<registry>/simfleet-controller:<tag>

make helm-package
helm install simfleet-controller dist/simfleet-controller-0.1.0.tgz \
  --namespace simfleet-controller-system --create-namespace \
  --set controllerManager.image.repository=<registry>/simfleet-controller \
  --set controllerManager.image.tag=<tag>
```

See the [chart README](charts/simfleet-controller/README.md) for every value.

To run a fleet of simulated DPUs against a real DPF cluster, start with the
[scale scenarios guide](docs/scenarios/README.md), then follow a scenario
such as [zero-trust passthrough](docs/scenarios/zero-trust-passthrough/README.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
