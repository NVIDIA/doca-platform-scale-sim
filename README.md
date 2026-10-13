# DOCA Platform Scale Sim

DOCA Platform Scale Sim runs fleets of simulated BlueField DPUs on Kubernetes,
so the [DOCA Platform Framework](https://github.com/NVIDIA/doca-platform) (DPF)
can be exercised at scale without the hardware. A `SimFleet` asks for a number
of DPUs; the controller backs each one with a KubeVirt VirtualMachine, a mock
Redfish BMC, and the DPF inventory objects (`DPUDevice`, `DPUNode`) that DPF
expects to find for a real device.

**Status:** migration in progress. The API is here; the controller, packaging,
end-to-end tests and CI follow in the next pull requests.

## Build and test

Requires Go 1.26.

```bash
make test
```

`make test` regenerates the CRDs and deepcopy code, then runs the unit tests.

## License

Apache License 2.0. See [LICENSE](LICENSE).
