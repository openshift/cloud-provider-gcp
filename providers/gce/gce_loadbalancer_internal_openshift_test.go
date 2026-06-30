//go:build !providerless

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

	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Test constants matching real OSD cluster topology from OCPBUGS-78471.
// node-instance-prefix and external-instance-groups-prefix are the same
// value (the infra name), and all node names are FQDNs.
const testInfraName = "test-cluster-a1b2c"

func fqdnName(short string) string {
	return fmt.Sprintf("%s.c.%s.internal", short, "test-project")
}

func igSelfLink(zone, name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/test-project/zones/%s/instanceGroups/%s", zone, name)
}

func TestFilterNodeObjectFromName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		nodes    []*v1.Node
		names    []string
		expected []string
	}{
		"FQDN node names match GCE short names": {
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-a-wnjp7")}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-b-s48dq")}},
			},
			names: []string{
				testInfraName + "-worker-a-wnjp7",
				testInfraName + "-worker-b-s48dq",
			},
			expected: []string{
				fqdnName(testInfraName + "-worker-a-wnjp7"),
				fqdnName(testInfraName + "-worker-b-s48dq"),
			},
		},
		"short node names still match": {
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: testInfraName + "-worker-a-wnjp7"}},
				{ObjectMeta: metav1.ObjectMeta{Name: testInfraName + "-worker-b-s48dq"}},
			},
			names:    []string{testInfraName + "-worker-a-wnjp7"},
			expected: []string{testInfraName + "-worker-a-wnjp7"},
		},
		"mixed FQDN and short names": {
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-a-wnjp7")}},
				{ObjectMeta: metav1.ObjectMeta{Name: testInfraName + "-worker-b-s48dq"}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-infra-a-zztd5")}},
			},
			names: []string{
				testInfraName + "-worker-a-wnjp7",
				testInfraName + "-worker-b-s48dq",
			},
			expected: []string{
				fqdnName(testInfraName + "-worker-a-wnjp7"),
				testInfraName + "-worker-b-s48dq",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			filtered := filterNodeObjectFromName(tc.nodes, tc.names)
			require.Len(t, filtered, len(tc.expected))
			for i, node := range filtered {
				assert.Equal(t, tc.expected[i], node.Name)
			}
		})
	}
}

func TestEvaluateExternalInstanceGroup(t *testing.T) {
	t.Parallel()

	vals := DefaultTestClusterValues()
	zone := vals.ZoneName
	igName := testInfraName + "-master-" + zone

	// gceHostNames is read-only so safe to share across parallel subtests.
	gceHostNames := sets.New(
		testInfraName+"-master-0",
		testInfraName+"-worker-a-wnjp7",
		testInfraName+"-infra-a-zztd5",
	)

	for name, tc := range map[string]struct {
		igInstances     []string // short instance names to add to the IG
		wantShouldReuse bool
		wantInSet       []string // instance names expected in the returned set; nil skips check
	}{
		"all IG instances are known nodes so IG should be reused": {
			igInstances:     []string{testInfraName + "-master-0"},
			wantShouldReuse: true,
			wantInSet:       []string{testInfraName + "-master-0"},
		},
		"bootstrap instance is ignored and IG with only known nodes should be reused": {
			igInstances:     []string{testInfraName + "-master-0", testInfraName + "-bootstrap"},
			wantShouldReuse: true,
			wantInSet:       []string{testInfraName + "-master-0"},
		},
		"empty instance group should not be reused": {
			igInstances:     []string{},
			wantShouldReuse: false,
		},
		"IG contains instance not in node list and not matching prefix so IG should not be reused": {
			igInstances:     []string{"other-cluster-master-0"},
			wantShouldReuse: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gce, err := fakeGCECloud(vals)
			require.NoError(t, err)
			gce.nodeInstancePrefix = testInfraName

			err = gce.CreateInstanceGroup(&compute.InstanceGroup{Name: igName}, zone)
			require.NoError(t, err)

			if len(tc.igInstances) > 0 {
				refs := make([]*compute.InstanceReference, len(tc.igInstances))
				for i, inst := range tc.igInstances {
					refs[i] = &compute.InstanceReference{
						Instance: fmt.Sprintf("zones/%s/instances/%s", zone, inst),
					}
				}
				err = gce.AddInstancesToInstanceGroup(igName, zone, refs)
				require.NoError(t, err)
			}

			ig, err := gce.GetInstanceGroup(igName, zone)
			require.NoError(t, err)

			shouldReuse, instanceNames, err := gce.evaluateExternalInstanceGroup(ig, zone, gceHostNames)
			require.NoError(t, err)
			assert.Equal(t, tc.wantShouldReuse, shouldReuse)
			if tc.wantInSet != nil {
				assert.ElementsMatch(t, tc.wantInSet, sets.List(instanceNames))
			}
		})
	}
}

func TestFilterNodesWithExistingExternalInstanceGroups(t *testing.T) {
	t.Parallel()

	vals := DefaultTestClusterValues()
	zoneA := vals.ZoneName          // us-central1-b
	zoneB := vals.SecondaryZoneName // us-central1-c

	type gceInstanceDef struct {
		zone string
		name string
	}
	type igDef struct {
		name      string
		zone      string
		instances []string // short instance names
	}

	for name, tc := range map[string]struct {
		externalInstanceGroupsPrefix string
		lbIGName                     string
		gceInstances                 []gceInstanceDef
		instanceGroups               []igDef
		nodes                        []*v1.Node
		wantIGLinks                  []string // full enumeration of expected IG self-links
		wantIncludedNodes            []string // full enumeration of node names expected in filtered result
		wantExcludedNodes            []string // node names expected NOT in filtered result
	}{
		"FQDN nodes covered by external master IGs are filtered out and workers and infra remain": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			gceInstances: []gceInstanceDef{
				{zoneA, testInfraName + "-master-0"},
				{zoneA, testInfraName + "-worker-a-wnjp7"},
				{zoneA, testInfraName + "-infra-a-zztd5"},
				{zoneB, testInfraName + "-master-1"},
				{zoneB, testInfraName + "-worker-b-s48dq"},
				{zoneB, testInfraName + "-infra-b-2bn6x"},
			},
			instanceGroups: []igDef{
				{testInfraName + "-master-" + zoneA, zoneA, []string{testInfraName + "-master-0"}},
				{testInfraName + "-master-" + zoneB, zoneB, []string{testInfraName + "-master-1"}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-master-0"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-a-wnjp7"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-infra-a-zztd5"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-master-1"), Labels: map[string]string{v1.LabelTopologyZone: zoneB}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-b-s48dq"), Labels: map[string]string{v1.LabelTopologyZone: zoneB}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-infra-b-2bn6x"), Labels: map[string]string{v1.LabelTopologyZone: zoneB}}},
			},
			wantIGLinks: []string{
				igSelfLink(zoneA, testInfraName+"-master-"+zoneA),
				igSelfLink(zoneB, testInfraName+"-master-"+zoneB),
			},
			wantIncludedNodes: []string{
				fqdnName(testInfraName + "-worker-a-wnjp7"),
				fqdnName(testInfraName + "-worker-b-s48dq"),
				fqdnName(testInfraName + "-infra-a-zztd5"),
				fqdnName(testInfraName + "-infra-b-2bn6x"),
			},
			wantExcludedNodes: []string{
				fqdnName(testInfraName + "-master-0"),
				fqdnName(testInfraName + "-master-1"),
			},
		},
		"no external instance groups prefix configured so all nodes remain": {
			externalInstanceGroupsPrefix: "",
			lbIGName:                     "k8s-ig--test-lb",
			gceInstances: []gceInstanceDef{
				{zoneA, testInfraName + "-master-0"},
				{zoneA, testInfraName + "-worker-a-wnjp7"},
			},
			instanceGroups: []igDef{},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-master-0"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-a-wnjp7"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
			},
			wantIGLinks: nil,
			wantIncludedNodes: []string{
				fqdnName(testInfraName + "-master-0"),
				fqdnName(testInfraName + "-worker-a-wnjp7"),
			},
		},
		// The LB IG name matches an existing external IG name. filterNodesWithExistingExternalInstanceGroups
		// skips the IG to avoid treating the LB's own IG as an external group to reuse.
		"external IG with same name as LB IG is skipped": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     testInfraName + "-master-" + zoneA,
			gceInstances: []gceInstanceDef{
				{zoneA, testInfraName + "-master-0"},
				{zoneA, testInfraName + "-worker-a-wnjp7"},
			},
			instanceGroups: []igDef{
				{testInfraName + "-master-" + zoneA, zoneA, []string{testInfraName + "-master-0"}},
			},
			nodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-master-0"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
				{ObjectMeta: metav1.ObjectMeta{Name: fqdnName(testInfraName + "-worker-a-wnjp7"), Labels: map[string]string{v1.LabelTopologyZone: zoneA}}},
			},
			wantIGLinks: nil,
			wantIncludedNodes: []string{
				fqdnName(testInfraName + "-master-0"),
				fqdnName(testInfraName + "-worker-a-wnjp7"),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gce, err := fakeGCECloud(vals)
			require.NoError(t, err)
			gce.nodeInstancePrefix = testInfraName
			gce.externalInstanceGroupsPrefix = tc.externalInstanceGroupsPrefix

			for _, inst := range tc.gceInstances {
				err = gce.InsertInstance(gce.ProjectID(), inst.zone, &compute.Instance{
					Name: inst.name,
					Tags: &compute.Tags{Items: []string{inst.name}},
					Zone: inst.zone,
				})
				require.NoError(t, err)
			}

			for _, ig := range tc.instanceGroups {
				err = gce.CreateInstanceGroup(&compute.InstanceGroup{Name: ig.name}, ig.zone)
				require.NoError(t, err)
				if len(ig.instances) > 0 {
					refs := make([]*compute.InstanceReference, len(ig.instances))
					for i, inst := range ig.instances {
						refs[i] = &compute.InstanceReference{
							Instance: fmt.Sprintf("zones/%s/instances/%s", ig.zone, inst),
						}
					}
					err = gce.AddInstancesToInstanceGroup(ig.name, ig.zone, refs)
					require.NoError(t, err)
				}
			}

			filteredNodes, existingIGLinks, err := gce.filterNodesWithExistingExternalInstanceGroups(tc.lbIGName, tc.nodes)
			require.NoError(t, err)

			assert.ElementsMatch(t, tc.wantIGLinks, existingIGLinks)
			assert.Len(t, filteredNodes, len(tc.wantIncludedNodes))

			filteredNames := sets.NewString()
			for _, n := range filteredNodes {
				filteredNames.Insert(n.Name)
			}
			for _, nodeName := range tc.wantIncludedNodes {
				assert.True(t, filteredNames.Has(nodeName), "expected %q in filtered nodes", nodeName)
			}
			for _, nodeName := range tc.wantExcludedNodes {
				assert.False(t, filteredNames.Has(nodeName), "expected %q excluded from filtered nodes", nodeName)
			}
		})
	}
}
