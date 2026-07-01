/*
Copyright 2025 The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-eng/openshift-tests-extension/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	gcecloud "k8s.io/cloud-provider-gcp/providers/gce"
	gcee2e "k8s.io/cloud-provider-gcp/test/e2e"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
)

const excludeFromLBLabel = "node.kubernetes.io/exclude-from-external-load-balancers"

const backendConvergenceTimeout = 3 * time.Minute

var _ = Describe("[cloud-provider-gcp-e2e] Internal LoadBalancer OpenShift behaviour", func() {
	f := framework.NewDefaultFramework("ilb-openshift")

	var cs clientset.Interface
	BeforeEach(func() {
		cs = f.ClientSet
	})

	// This tests validates that the OpenShift carry patch to re-use existing
	// instance groups correctly honours the
	// node.kubernetes.io/exclude-from-external-load-balancers label.
	//
	// 1. Label all control plane nodes with node.kubernetes.io/exclude-from-external-load-balancers
	// 2. Create an internal LoadBalancer service.
	// 3. Inspect the GCE backend service's instance groups to verify that
	//    no control plane instance appears in any backend.
	f.It("should not include control plane nodes in backends when control plane nodes have the exclude-from-external-load-balancers label", f.WithSlow(), func(ctx context.Context) {
		gceCloud, err := gcee2e.GetGCECloud()
		framework.ExpectNoError(err, "failed to get GCE cloud provider")

		// Ensure control plane nodes are labeled with excludeFromLBLabel.
		controlPlaneInstanceNames := labelControlPlaneNodes(ctx, cs)

		// Create an internal LoadBalancer service.
		lbIP := createInternalLoadBalancerService(ctx, cs, f.Namespace.Name, "ilb-exclude-control-plane-test")

		region := gceCloud.Region()

		// Find the forwarding rule for our service by matching the IP.
		fwdRules, err := gceCloud.ListRegionForwardingRules(region)
		framework.ExpectNoError(err, "failed to list region forwarding rules")

		var backendServiceURL string
		for _, rule := range fwdRules {
			if rule.IPAddress == lbIP && rule.LoadBalancingScheme == "INTERNAL" {
				backendServiceURL = rule.BackendService
				framework.Logf("Found forwarding rule %q for IP %s, backend service: %s",
					rule.Name, lbIP, backendServiceURL)
				break
			}
		}
		Expect(backendServiceURL).NotTo(BeEmpty(),
			"could not find INTERNAL forwarding rule for IP %s", lbIP)

		// Extract the backend service name from the URL and fetch it.
		bsParts := strings.Split(backendServiceURL, "/")
		backendServiceName := bsParts[len(bsParts)-1]

		By("waiting for backend reconciliation to exclude control plane nodes", func() {
			err := wait.PollUntilContextTimeout(ctx, e2eservice.LoadBalancerPollInterval, backendConvergenceTimeout, true, func(ctx context.Context) (bool, error) {
				backendsReady, controlPlaneBackends, err := backendServiceControlPlaneInstances(gceCloud, region, backendServiceName, controlPlaneInstanceNames)
				if err != nil {
					return false, err
				}
				if !backendsReady {
					framework.Logf("Backend service %s has no backends yet", backendServiceName)
					return false, nil
				}
				if len(controlPlaneBackends) > 0 {
					framework.Logf(
						"Still waiting for control plane instances to leave backend service %s: %s",
						backendServiceName,
						strings.Join(controlPlaneBackends, ", "),
					)
					return false, nil
				}
				return true, nil
			})
			framework.ExpectNoError(err, "timed out waiting for backend service %s to have backends without control plane instances", backendServiceName)
		})

		framework.Logf("Verified: no control plane instances are present in any backend instance group")
	})
})

func labelControlPlaneNodes(ctx context.Context, cs clientset.Interface) []string {
	GinkgoHelper()

	controlPlaneInstanceNames := []string{}

	By("labeling control plane nodes with "+excludeFromLBLabel, func() {
		// Get all control plane nodes.
		controlPlaneNodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/master",
		})
		framework.ExpectNoError(err)
		if len(controlPlaneNodes.Items) == 0 {
			framework.Failf("no control plane nodes found")
		}

		// Label all control plane nodes with the exclude label, saving original state.
		for i := range controlPlaneNodes.Items {
			node := &controlPlaneNodes.Items[i]

			shortName := strings.Split(node.Name, ".")[0]
			controlPlaneInstanceNames = append(controlPlaneInstanceNames, shortName)

			if _, has := node.Labels[excludeFromLBLabel]; !has {
				patchData := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:""}}}`, excludeFromLBLabel))
				_, err := cs.CoreV1().Nodes().Patch(ctx, node.Name,
					types.MergePatchType, patchData, metav1.PatchOptions{})
				framework.ExpectNoError(err, "failed to add exclude label to node %s", node.Name)
				framework.Logf("Added %s label to node %s", excludeFromLBLabel, node.Name)

				DeferCleanup(func(ctx context.Context) {
					By("removing "+excludeFromLBLabel+" label from node "+node.Name, func() {
						patchData := fmt.Appendf(nil, `{"metadata":{"labels":{%q:null}}}`, excludeFromLBLabel)
						_, err := cs.CoreV1().Nodes().Patch(ctx, node.Name,
							types.MergePatchType, patchData, metav1.PatchOptions{})
						framework.ExpectNoError(err, "failed to remove exclude label from node %s", node.Name)
						framework.Logf("Removed %s label from node %s", excludeFromLBLabel, node.Name)
					})
				})
			} else {
				framework.Logf("Node %s already has %s label", node.Name, excludeFromLBLabel)
			}
		}
	})

	return controlPlaneInstanceNames
}

func createInternalLoadBalancerService(ctx context.Context, cs clientset.Interface, namespace, name string) string {
	GinkgoHelper()

	By("creating an internal LoadBalancer service", func() {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Annotations: map[string]string{
					"networking.gke.io/load-balancer-type": "Internal",
				},
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeLoadBalancer,
				Ports: []corev1.ServicePort{{
					Port:       80,
					TargetPort: intstr.FromInt32(8080),
					Protocol:   corev1.ProtocolTCP,
				}},
				// Use a selector that won't match any pods. We don't need
				// actual traffic; we only need the CCM to create the GCE
				// resources so we can inspect the backend instance groups.
				Selector: map[string]string{
					"app": "ilb-exclude-control-plane-no-match",
				},
			},
		}
		_, err := cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create internal LB service")
		framework.Logf("Created internal LB service %s/%s", namespace, name)
	})

	// Wait for the service to get a load balancer IP, which signals
	// that the CCM has created the GCE forwarding rule and backends.
	var lbIP string
	By("waiting for the service to get a load balancer IP", func() {
		loadBalancerCreateTimeout := e2eservice.GetServiceLoadBalancerCreationTimeout(ctx, cs)
		err := wait.PollUntilContextTimeout(ctx, e2eservice.LoadBalancerPollInterval, loadBalancerCreateTimeout, true, func(ctx context.Context) (bool, error) {
			current, err := cs.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if len(current.Status.LoadBalancer.Ingress) > 0 {
				lbIP = current.Status.LoadBalancer.Ingress[0].IP
				return true, nil
			}
			return false, nil
		})
		framework.ExpectNoError(err, "timed out waiting for ILB IP assignment")
		framework.Logf("Internal LB service got IP: %s", lbIP)
	})

	return lbIP
}

func backendServiceControlPlaneInstances(gceCloud *gcecloud.Cloud, region, backendServiceName string, controlPlaneInstanceNames []string) (bool, []string, error) {
	GinkgoHelper()

	bs, err := gceCloud.GetRegionBackendService(backendServiceName, region)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get backend service %s: %w", backendServiceName, err)
	}
	if len(bs.Backends) == 0 {
		return false, nil, nil
	}

	controlPlaneSet := sets.New(controlPlaneInstanceNames...)

	var controlPlaneBackends []string
	for _, backend := range bs.Backends {
		igURL := backend.Group
		// Instance group URL format:
		//   .../projects/<project>/zones/<zone>/instanceGroups/<name>
		parts := strings.Split(igURL, "/")
		if len(parts) < 4 {
			return false, nil, fmt.Errorf("unexpected instance group URL format: %s", igURL)
		}
		igName := parts[len(parts)-1]
		igZone := parts[len(parts)-3]

		framework.Logf("Checking backend instance group %s in zone %s", igName, igZone)

		instances, err := gceCloud.ListInstancesInInstanceGroup(igName, igZone, "ALL")
		if err != nil {
			return false, nil, fmt.Errorf("failed to list instances in IG %s/%s: %w", igZone, igName, err)
		}

		for _, inst := range instances {
			instParts := strings.Split(inst.Instance, "/")
			instName := instParts[len(instParts)-1]
			if controlPlaneSet.Has(instName) {
				controlPlaneBackends = append(controlPlaneBackends, fmt.Sprintf("%s in %s/%s", instName, igZone, igName))
			}
		}
	}

	sort.Strings(controlPlaneBackends)
	return true, controlPlaneBackends, nil
}
