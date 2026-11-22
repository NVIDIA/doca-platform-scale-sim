module github.com/NVIDIA/doca-platform-scale-sim/pkg/mock-ovs

go 1.26.0

require (
	antrea.io/antrea v1.15.2
	github.com/go-logr/logr v1.4.4
	github.com/go-logr/stdr v1.2.2
	// 15f1775b1baa is the commit of the v30.0.0-latest-stable tag. Go cannot require that
	// tag by name: a v30 version needs a module path ending in /v30.
	github.com/nvidia/doca-platform v0.0.0-20261114053626-15f1775b1baa
	github.com/ovn-org/libovsdb v0.7.0
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4
)

require (
	antrea.io/libOpenflow v0.17.0 // indirect
	antrea.io/ofnet v0.12.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cenkalti/hub v1.0.2 // indirect
	github.com/cenkalti/rpc2 v1.0.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/contiv/libovsdb v0.0.0-20170227191248-d0061a53e358 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/orcaman/concurrent-map/v2 v2.0.1 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.17.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/mock v0.5.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	k8s.io/apimachinery v0.35.4 // indirect
	k8s.io/component-base v0.35.4 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
	sigs.k8s.io/controller-runtime v0.22.3 // indirect
)
