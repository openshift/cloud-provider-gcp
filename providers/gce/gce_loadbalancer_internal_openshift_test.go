//go:build !providerless
// +build !providerless

/*
Copyright 2026 Red Hat, Inc.

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

package gce

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	compute "google.golang.org/api/compute/v1"
)

// testInfraName is the cluster infrastructure name, used as both the
// node-instance-prefix and external-instance-groups-prefix, as in real OCP
// GCP clusters.
const testInfraName = "test-cluster-a1b2c"

func igSelfLink(projectID, zone, name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instanceGroups/%s", projectID, zone, name)
}

// TestEnsureInternalInstanceGroupsExternalIGReuse tests the logic that decides
// whether a pre-existing external (installer-managed) instance group should be
// reused by the CCM's internal load balancer, or whether the CCM should create
// its own instance group.
//
// In particular it covers:
//   - OCPBUGS-35256: the bootstrap machine appears in a master IG during
//     installation; it must not prevent the IG from being reused.
//   - OCPBUGS-84569: masters labelled with
//     node.kubernetes.io/exclude-from-external-load-balancers are filtered by
//     the CCM framework before reaching this code.  The master IG must not be
//     reused in that case, otherwise traffic is sent to control-plane machines.
func TestEnsureInternalInstanceGroupsExternalIGReuse(t *testing.T) {
	t.Parallel()

	vals := DefaultTestClusterValues()
	zone := vals.ZoneName

	masterIGName := testInfraName + "-master-" + zone
	lbIGName := makeInstanceGroupName(vals.ClusterID)

	for name, tc := range map[string]struct {
		igInstances  []string // short instance names to place in the external IG
		lbNodes      []string // short names of nodes passed to ensureInternalInstanceGroups
		wantMasterIG bool     // whether the master IG self-link should appear in the result
	}{
		"all IG instances are known nodes so IG should be reused": {
			igInstances:  []string{testInfraName + "-master-0"},
			lbNodes:      []string{testInfraName + "-master-0"},
			wantMasterIG: true,
		},
		"bootstrap instance is ignored and IG with only known nodes should be reused": {
			igInstances:  []string{testInfraName + "-master-0", testInfraName + "-bootstrap"},
			lbNodes:      []string{testInfraName + "-master-0"},
			wantMasterIG: true,
		},
		"empty instance group should not be reused": {
			igInstances:  []string{},
			lbNodes:      []string{testInfraName + "-master-0"},
			wantMasterIG: false,
		},
		"IG contains instance not in node list so IG should not be reused": {
			igInstances:  []string{"other-cluster-master-0"},
			lbNodes:      []string{testInfraName + "-master-0"},
			wantMasterIG: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gce, err := fakeGCECloud(vals)
			require.NoError(t, err)
			gce.nodeInstancePrefix = testInfraName
			gce.externalInstanceGroupsPrefix = testInfraName

			// Insert GCE instances and build Node objects for the nodes that
			// will be passed to ensureInternalInstanceGroups.
			nodes, err := createAndInsertNodes(gce, tc.lbNodes, zone)
			require.NoError(t, err)

			// Create the external master IG and populate it.
			err = gce.CreateInstanceGroup(&compute.InstanceGroup{Name: masterIGName}, zone)
			require.NoError(t, err)
			if len(tc.igInstances) > 0 {
				refs := make([]*compute.InstanceReference, len(tc.igInstances))
				for i, inst := range tc.igInstances {
					refs[i] = &compute.InstanceReference{
						Instance: fmt.Sprintf("zones/%s/instances/%s", zone, inst),
					}
				}
				err = gce.AddInstancesToInstanceGroup(masterIGName, zone, refs)
				require.NoError(t, err)
			}

			igLinks, err := gce.ensureInternalInstanceGroups(lbIGName, nodes)
			require.NoError(t, err)

			masterIGLink := igSelfLink(vals.ProjectID, zone, masterIGName)
			if tc.wantMasterIG {
				assert.Contains(t, igLinks, masterIGLink, "expected master IG to be reused")
			} else {
				assert.NotContains(t, igLinks, masterIGLink, "expected master IG not to be reused")
			}
		})
	}
}
