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
	"slices"
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

func igSelfLink(zone, role string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/test-project/zones/%s/instanceGroups/%s", zone, testInfraName+"-"+role+"-"+zone)
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

func TestFilterNodesWithExistingExternalInstanceGroups(t *testing.T) {
	t.Parallel()

	vals := DefaultTestClusterValues()
	zoneA := vals.ZoneName          // us-central1-b
	zoneB := vals.SecondaryZoneName // us-central1-c

	type testNode struct{ name, zone string }

	testNodes := []testNode{
		{name: "master-0", zone: zoneA},
		{name: "worker-a-wnjp7", zone: zoneA},
		{name: "infra-a-zztd5", zone: zoneA},
		{name: "master-1", zone: zoneB},
		{name: "worker-b-s48dq", zone: zoneB},
		{name: "infra-b-2bn6x", zone: zoneB},
	}

	nodesFn := func(fqdn bool) []*v1.Node {
		r := make([]*v1.Node, len(testNodes))
		for i, t := range testNodes {
			r[i] = &v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:   testInfraName + "-" + t.name,
				Labels: map[string]string{v1.LabelTopologyZone: t.zone}}}
			if fqdn {
				r[i].Name = fqdnName(r[i].Name)
			}
		}

		return r
	}

	// setupFake creates a fake GCE cloud with instances and master IGs in both zones.
	// igZoneAInstances allows the set of instances in the zoneA master IG to be overridden.
	setupFake := func(t *testing.T, prefix string, igZoneAInstances []string) *Cloud {
		t.Helper()
		gce, err := fakeGCECloud(vals)
		require.NoError(t, err)
		gce.nodeInstancePrefix = testInfraName
		gce.externalInstanceGroupsPrefix = prefix

		// Create GCE instances for the test nodes
		for _, testNode := range testNodes {
			require.NoError(t, gce.InsertInstance(gce.ProjectID(), testNode.zone, &compute.Instance{
				Name: testInfraName + "-" + testNode.name, Tags: &compute.Tags{Items: []string{testNode.name}}, Zone: testNode.zone,
			}))
		}

		// Create GCE instances for any additional instances specified for the first instance group.
		for _, inst := range igZoneAInstances {
			if !slices.ContainsFunc(testNodes, func(t testNode) bool { return testInfraName+"-"+t.name == inst }) {
				require.NoError(t, gce.InsertInstance(gce.ProjectID(), zoneA, &compute.Instance{
					Name: inst, Tags: &compute.Tags{Items: []string{inst}}, Zone: zoneA,
				}))
			}
		}

		// By default the first instance group contains the first master
		// instance, but it can be overridden.
		if igZoneAInstances == nil {
			igZoneAInstances = []string{testInfraName + "-master-0"}
		}

		// Create a GCE instance group for each test zone
		for _, ig := range []struct {
			zone, name string
			members    []string
		}{
			{zoneA, testInfraName + "-master-" + zoneA, igZoneAInstances},
			{zoneB, testInfraName + "-master-" + zoneB, []string{testInfraName + "-master-1"}},
		} {
			require.NoError(t, gce.CreateInstanceGroup(&compute.InstanceGroup{Name: ig.name}, ig.zone))
			for _, member := range ig.members {
				require.NoError(t, gce.AddInstancesToInstanceGroup(ig.name, ig.zone, []*compute.InstanceReference{
					{Instance: fmt.Sprintf("zones/%s/instances/%s", ig.zone, member)},
				}))
			}
		}

		return gce
	}

	nodeNames := func(nodes []*v1.Node) sets.Set[string] {
		names := sets.New[string]()
		for _, n := range nodes {
			names.Insert(n.Name)
		}
		return names
	}

	for name, tc := range map[string]struct {
		useFQDNNodeNames             bool
		externalInstanceGroupsPrefix string
		lbIGName                     string
		igZoneAInstances             []string // instances in zoneA master IG; nil = default (master-0)
		excludeNodeNames             []string // nodes to remove before calling the function, simulating CCM pre-filtering
		wantIGs                      []string
		wantExcludedNodes            []string
	}{
		"FQDN nodes covered by external master IGs are filtered out and workers and infra remain": {
			useFQDNNodeNames:             true,
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			wantIGs: []string{
				igSelfLink(zoneA, "master"),
				igSelfLink(zoneB, "master"),
			},
			wantExcludedNodes: []string{fqdnName(testInfraName + "-master-0"), fqdnName(testInfraName + "-master-1")},
		},
		"no external instance groups prefix configured so all nodes remain": {
			externalInstanceGroupsPrefix: "",
			lbIGName:                     "k8s-ig--test-lb",
			wantIGs:                      nil,
		},
		// The LB IG name matches an existing external IG name in zoneA.
		// That IG is skipped; only the zoneB master IG is reused.
		"external IG with same name as LB IG is skipped": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     testInfraName + "-master-" + zoneA,
			wantIGs:                      []string{igSelfLink(zoneB, "master")},
			wantExcludedNodes:            []string{testInfraName + "-master-1"},
		},
		"bootstrap instance in master IG is ignored and IG is still reused": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			igZoneAInstances:             []string{testInfraName + "-master-0", testInfraName + "-bootstrap"},
			wantIGs: []string{
				igSelfLink(zoneA, "master"),
				igSelfLink(zoneB, "master"),
			},
			wantExcludedNodes: []string{testInfraName + "-master-0", testInfraName + "-master-1"},
		},
		"bootstrap-only instance group is not reused": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			igZoneAInstances:             []string{testInfraName + "-bootstrap"},
			wantIGs:                      []string{igSelfLink(zoneB, "master")},
			wantExcludedNodes:            []string{testInfraName + "-master-1"},
		},
		"empty instance group is not reused": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			igZoneAInstances:             []string{},
			wantIGs:                      []string{igSelfLink(zoneB, "master")},
			wantExcludedNodes:            []string{testInfraName + "-master-1"},
		},
		"IG with unknown instance is not reused": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			igZoneAInstances:             []string{"other-cluster-master-0"},
			wantIGs:                      []string{igSelfLink(zoneB, "master")},
			wantExcludedNodes:            []string{testInfraName + "-master-1"},
		},
		"all IG instances are known nodes so IG is reused": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			wantIGs: []string{
				igSelfLink(zoneA, "master"),
				igSelfLink(zoneB, "master"),
			},
			wantExcludedNodes: []string{testInfraName + "-master-0", testInfraName + "-master-1"},
		},
		// Masters labelled node.kubernetes.io/exclude-from-external-load-balancers are
		// filtered by the CCM framework before the cloud provider is called. The master
		// instance groups still exist on GCP and contain the master instances, but because
		// gceHostNamesInZone is derived only from the nodes passed in (workers+infra),
		// HasAll fails for those IGs and they must not be reused. The old allHaveNodePrefix
		// fallback would have returned shouldReuse=true here (all masters share the infra
		// prefix), incorrectly sending traffic to the control plane.
		"masters pre-filtered by CCM label: master IGs not reused, workers and infra get internal IGs": {
			externalInstanceGroupsPrefix: testInfraName,
			lbIGName:                     "k8s-ig--test-lb",
			excludeNodeNames: []string{
				testInfraName + "-master-0",
				testInfraName + "-master-1",
			},
			wantIGs:           nil, // master IGs must not be reused
			wantExcludedNodes: nil, // all remaining nodes (workers+infra) need internal IGs
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gce := setupFake(t, tc.externalInstanceGroupsPrefix, tc.igZoneAInstances)
			nodes := nodesFn(tc.useFQDNNodeNames)
			if len(tc.excludeNodeNames) > 0 {
				excludeSet := sets.New(tc.excludeNodeNames...)
				var kept []*v1.Node
				for _, n := range nodes {
					if !excludeSet.Has(n.Name) {
						kept = append(kept, n)
					}
				}
				nodes = kept
			}
			filteredNodes, existingIGLinks, err := gce.filterNodesWithExistingExternalInstanceGroups(tc.lbIGName, nodes)
			require.NoError(t, err)

			assert.ElementsMatch(t, tc.wantIGs, existingIGLinks)

			// Assert that filteredNodes contains all nodes except the excluded ones.
			allNodeNames := nodeNames(nodes)
			wantIncluded := allNodeNames.Difference(sets.New(tc.wantExcludedNodes...))
			assert.ElementsMatch(t, wantIncluded.UnsortedList(), nodeNames(filteredNodes).UnsortedList())
		})
	}
}

func TestEvaluateExternalInstanceGroup(t *testing.T) {
	t.Parallel()
	vals := DefaultTestClusterValues()
	gce, err := fakeGCECloud(vals)
	require.NoError(t, err)
	gce.nodeInstancePrefix = testInfraName
	zone := vals.ZoneName

	// CAPG creates one master IG per zone. During install, the bootstrap node
	// lands in the same IG as the master in that zone (e.g. both master-0 and
	// bootstrap in <infra>-master-<zone>).
	masterIGName := testInfraName + "-master-" + zone

	// IG contains only the master. shouldReuse=true via hasAll.
	err = gce.CreateInstanceGroup(&compute.InstanceGroup{Name: masterIGName}, zone)
	require.NoError(t, err)
	err = gce.AddInstancesToInstanceGroup(masterIGName, zone, []*compute.InstanceReference{
		{Instance: fmt.Sprintf("zones/%s/instances/%s-master-0", zone, testInfraName)},
	})
	require.NoError(t, err)
	masterIG, err := gce.GetInstanceGroup(masterIGName, zone)
	require.NoError(t, err)

	gceHostNames := sets.New(
		testInfraName+"-master-0",
		testInfraName+"-worker-a-wnjp7",
		testInfraName+"-infra-a-zztd5",
	)
	shouldReuse, instanceNames, err := gce.evaluateExternalInstanceGroup(masterIG, zone, gceHostNames)
	require.NoError(t, err)
	assert.True(t, shouldReuse, "should reuse when all IG instances are in zone node list")
	assert.True(t, instanceNames.Has(testInfraName+"-master-0"))

	// During CAPG install (OCPBUGS-35256): bootstrap node is in the same master
	// IG as master-0 (same zone). Bootstrap is not a k8s node so hasAll=false,
	// but all instances share the infra prefix so allHavePrefix=true.
	err = gce.AddInstancesToInstanceGroup(masterIGName, zone, []*compute.InstanceReference{
		{Instance: fmt.Sprintf("zones/%s/instances/%s-bootstrap", zone, testInfraName)},
	})
	require.NoError(t, err)

	shouldReuse, _, err = gce.evaluateExternalInstanceGroup(masterIG, zone, gceHostNames)
	require.NoError(t, err)
	assert.True(t, shouldReuse, "should reuse master IG even with bootstrap node present")
}

func TestFilterNodesWithExistingExternalInstanceGroups_FQDNNodes(t *testing.T) {
	t.Parallel()
	vals := DefaultTestClusterValues()
	gce, err := fakeGCECloud(vals)
	require.NoError(t, err)

	// Both prefixes are the infra name, matching real cloud config
	gce.nodeInstancePrefix = testInfraName
	gce.externalInstanceGroupsPrefix = testInfraName

	zoneA := vals.ZoneName          // us-central1-b
	zoneB := vals.SecondaryZoneName // us-central1-c

	// GCE instances (short names) across two zones — masters, workers, infra
	zoneAInstances := []string{
		testInfraName + "-master-0",
		testInfraName + "-worker-a-wnjp7",
		testInfraName + "-infra-a-zztd5",
	}
	zoneBInstances := []string{
		testInfraName + "-master-1",
		testInfraName + "-worker-b-s48dq",
		testInfraName + "-infra-b-2bn6x",
	}
	for _, name := range zoneAInstances {
		err = gce.InsertInstance(gce.ProjectID(), zoneA, &compute.Instance{
			Name: name,
			Tags: &compute.Tags{Items: []string{name}},
			Zone: zoneA,
		})
		require.NoError(t, err)
	}
	for _, name := range zoneBInstances {
		err = gce.InsertInstance(gce.ProjectID(), zoneB, &compute.Instance{
			Name: name,
			Tags: &compute.Tags{Items: []string{name}},
			Zone: zoneB,
		})
		require.NoError(t, err)
	}

	// External master IGs per zone (created by installer, match externalInstanceGroupsPrefix)
	for _, z := range []struct {
		zone   string
		master string
	}{
		{zoneA, testInfraName + "-master-0"},
		{zoneB, testInfraName + "-master-1"},
	} {
		igName := testInfraName + "-master-" + z.zone
		err = gce.CreateInstanceGroup(&compute.InstanceGroup{Name: igName}, z.zone)
		require.NoError(t, err)
		err = gce.AddInstancesToInstanceGroup(igName, z.zone, []*compute.InstanceReference{
			{Instance: fmt.Sprintf("zones/%s/instances/%s", z.zone, z.master)},
		})
		require.NoError(t, err)
	}

	// Nodes with FQDN names across both zones — masters, workers, infra
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-master-0"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneA},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-worker-a-wnjp7"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneA},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-infra-a-zztd5"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneA},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-master-1"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneB},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-worker-b-s48dq"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneB},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   fqdnName(testInfraName + "-infra-b-2bn6x"),
			Labels: map[string]string{v1.LabelTopologyZone: zoneB},
		}},
	}

	lbIGName := "k8s-ig--test-lb"
	filteredNodes, existingIGLinks, err := gce.filterNodesWithExistingExternalInstanceGroups(lbIGName, nodes)
	require.NoError(t, err)

	// Masters filtered out (covered by external IGs), workers + infra remain
	assert.Len(t, existingIGLinks, 2, "one master IG per zone should be reused")
	assert.Len(t, filteredNodes, 4, "workers and infra nodes should remain for internal IG creation")

	filteredNames := sets.NewString()
	for _, n := range filteredNodes {
		filteredNames.Insert(n.Name)
	}
	assert.True(t, filteredNames.Has(fqdnName(testInfraName+"-worker-a-wnjp7")))
	assert.True(t, filteredNames.Has(fqdnName(testInfraName+"-worker-b-s48dq")))
	assert.True(t, filteredNames.Has(fqdnName(testInfraName+"-infra-a-zztd5")))
	assert.True(t, filteredNames.Has(fqdnName(testInfraName+"-infra-b-2bn6x")))
	assert.False(t, filteredNames.Has(fqdnName(testInfraName+"-master-0")))
	assert.False(t, filteredNames.Has(fqdnName(testInfraName+"-master-1")))
}
