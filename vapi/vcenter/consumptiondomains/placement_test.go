// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package consumptiondomains_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/vcenter/consumptiondomains/associations"
	"github.com/vmware/govmomi/vapi/vcenter/consumptiondomains/zones"
	"github.com/vmware/govmomi/vim25"

	_ "github.com/vmware/govmomi/vapi/simulator"
	_ "github.com/vmware/govmomi/vapi/vcenter/consumptiondomains/simulator"
)

type testZonalSpec struct {
	Zone            string   `json:"zone"`
	AllowedClusters []string `json:"allowed_clusters,omitempty"`
}

type testPlacementSpec struct {
	Operation  string          `json:"operation"`
	ZonalSpecs []testZonalSpec `json:"zonal_specs"`
}

type testPlacementResult struct {
	ZonalPlacementResults []struct {
		Zone    string `json:"zone"`
		Status  string `json:"status"`
		Results []struct {
			Cluster string `json:"cluster"`
			Score   int64  `json:"score"`
		} `json:"results"`
	} `json:"zonal_placement_results"`
}

func place(ctx context.Context, t *testing.T, c *rest.Client, zs testZonalSpec) testPlacementResult {
	t.Helper()
	u := c.Resource("/api/vcenter/consumption-domains/zones/placement/namespaces").WithParam("action", "place")
	spec := testPlacementSpec{Operation: "INITIAL_PLACEMENT", ZonalSpecs: []testZonalSpec{zs}}
	var res testPlacementResult
	require.NoError(t, c.Do(ctx, u.Request(http.MethodPost, spec), &res))
	require.Len(t, res.ZonalPlacementResults, 1)
	return res
}

func TestPlaceNamespaces(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		c := rest.NewClient(vc)
		require.NoError(t, c.Login(ctx, simulator.DefaultLogin))

		// No allowed clusters and an unknown zone: every inventory cluster.
		res := place(ctx, t, c, testZonalSpec{Zone: "zone-a"})
		zr := res.ZonalPlacementResults[0]
		assert.Equal(t, "zone-a", zr.Zone)
		assert.Equal(t, "PLACEMENT_SUCCESS", zr.Status)
		require.NotEmpty(t, zr.Results)
		assert.Equal(t, "domain-c", zr.Results[0].Cluster[:len("domain-c")])

		// Allowed clusters win.
		res = place(ctx, t, c, testZonalSpec{Zone: "zone-a", AllowedClusters: []string{"domain-c42"}})
		assert.Equal(t, "domain-c42", res.ZonalPlacementResults[0].Results[0].Cluster)

		// A known zone uses its associations.
		zm := zones.NewManager(c)
		_, err := zm.CreateZone(zones.CreateSpec{Zone: "zone-b"})
		require.NoError(t, err)
		err = associations.NewManager(c).AddAssociations("zone-b", "domain-c7")
		require.NoError(t, err)
		res = place(ctx, t, c, testZonalSpec{Zone: "zone-b"})
		require.Len(t, res.ZonalPlacementResults[0].Results, 1)
		assert.Equal(t, "domain-c7", res.ZonalPlacementResults[0].Results[0].Cluster)
	})
}
