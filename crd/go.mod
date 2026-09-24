module k8s.io/cloud-provider-gcp/crd

go 1.16

require (
	github.com/onsi/ginkgo v1.16.4 // indirect
	github.com/onsi/gomega v1.13.0 // indirect
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b // indirect
	k8s.io/apimachinery v0.21.1
	k8s.io/client-go v0.21.1
)

replace golang.org/x/net => github.com/openshift-sustaining/net v0.35.0-sec.4
