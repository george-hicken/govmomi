// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"net/http"
	"slices"

	vapi "github.com/vmware/govmomi/vapi/simulator"
)

// The namespace placement types mirror the wire shape of
// POST /api/vcenter/consumption-domains/zones/placement/namespaces?action=place.
// The client binding is not part of govmomi, so the simulator declares them.

type placementSpec struct {
	Operation  string               `json:"operation"`
	ZonalSpecs []zonalPlacementSpec `json:"zonal_specs"`
}

type zonalPlacementSpec struct {
	Zone                  string   `json:"zone"`
	NamespaceResourcePool string   `json:"namespace_resource_pool,omitempty"`
	AllowedClusters       []string `json:"allowed_clusters,omitempty"`
}

type placementResultItem struct {
	Cluster string `json:"cluster"`
	Score   int64  `json:"score"`
}

type zonalPlacementResult struct {
	Zone    string                `json:"zone"`
	Results []placementResultItem `json:"results,omitempty"`
	Status  string                `json:"status"`
}

type placementResult struct {
	ZonalPlacementResults []zonalPlacementResult `json:"zonal_placement_results"`
}

// placeNamespaces suggests clusters for each zonal spec. Candidates are the
// spec's allowed clusters, else the zone's cluster associations, else every
// cluster in the inventory. A zone with no candidate is a PLACEMENT_FAILURE.
func (h *Handler) placeNamespaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Query().Get("action") != "place" {
		vapi.ApiErrorNotFound(w)
		return
	}

	var spec placementSpec
	if !vapi.Decode(r, w, &spec) {
		return
	}
	if len(spec.ZonalSpecs) == 0 {
		vapi.ApiErrorInvalidArgument(w)
		return
	}

	result := placementResult{ZonalPlacementResults: make([]zonalPlacementResult, 0, len(spec.ZonalSpecs))}
	for _, zs := range spec.ZonalSpecs {
		zr := zonalPlacementResult{Zone: zs.Zone, Status: "PLACEMENT_FAILURE"}
		for i, c := range h.placementCandidates(zs) {
			zr.Results = append(zr.Results, placementResultItem{Cluster: c, Score: int64(100 - i)})
			zr.Status = "PLACEMENT_SUCCESS"
		}
		result.ZonalPlacementResults = append(result.ZonalPlacementResults, zr)
	}
	vapi.StatusOK(w, result)
}

func (h *Handler) placementCandidates(zs zonalPlacementSpec) []string {
	if len(zs.AllowedClusters) > 0 {
		return zs.AllowedClusters
	}
	if d, ok := h.data[zs.Zone]; ok && len(d.Associations) > 0 {
		return d.Associations
	}
	var ids []string
	if h.reg != nil {
		for _, e := range h.reg.All("ClusterComputeResource") {
			ids = append(ids, e.Reference().Value)
		}
	}
	slices.Sort(ids)
	return ids
}
