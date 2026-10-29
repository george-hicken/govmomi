// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/cns"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	vim25types "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vslm"
)

const (
	testLabel = "testLabel"
	testValue = "testValue"
)

func TestSimulator(t *testing.T) {
	ctx := context.Background()

	model := simulator.VPX()
	defer model.Remove()

	var err error

	if err = model.Create(); err != nil {
		t.Fatal(err)
	}

	s := model.Service.NewServer()
	defer s.Close()

	model.Service.RegisterSDK(New())

	c, err := govmomi.NewClient(ctx, s.URL, true)
	if err != nil {
		t.Fatal(err)
	}

	cnsClient, err := cns.NewClient(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	// Query
	queryFilter := cnstypes.CnsQueryFilter{}
	queryResult, err := cnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		t.Fatal(err)
	}
	existingNumDisks := len(queryResult.Volumes)

	// Get a simulator DS
	datastore := model.Map().Any("Datastore").(*simulator.Datastore)

	// Create volume for static provisioning
	var capacityInMb int64 = 1024

	task, err := vslm.NewObjectManager(c.Client).CreateDisk(ctx, vim25types.VslmCreateSpec{
		Name:         "vcsim-test",
		CapacityInMB: capacityInMb,
		BackingSpec: &vim25types.VslmCreateSpecDiskFileBackingSpec{
			VslmCreateSpecBackingSpec: vim25types.VslmCreateSpecBackingSpec{
				Datastore: datastore.Reference(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := task.WaitForResult(ctx)
	if err != nil {
		t.Fatal(err)
	}

	createSpecListForStaticProvision := []cnstypes.CnsVolumeCreateSpec{
		{
			Name:       "test",
			VolumeType: "TestVolumeType",
			Datastores: []vim25types.ManagedObjectReference{
				datastore.Self,
			},
			BackingObjectDetails: &cnstypes.CnsBlockBackingDetails{
				CnsBackingObjectDetails: cnstypes.CnsBackingObjectDetails{
					CapacityInMb: capacityInMb,
				},
				BackingDiskId: res.Result.(vim25types.VStorageObject).Config.Id.Id,
			},
			Profile: []vim25types.BaseVirtualMachineProfileSpec{
				&vim25types.VirtualMachineDefinedProfileSpec{
					ProfileId: uuid.New().String(),
				},
			},
		},
	}
	createTask, err := cnsClient.CreateVolume(ctx, createSpecListForStaticProvision)
	if err != nil {
		t.Fatal(err)
	}

	createTaskInfo, err := cns.GetTaskInfo(ctx, createTask)
	if err != nil {
		t.Fatal(err)
	}

	createTaskResult, err := cns.GetTaskResult(ctx, createTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if createTaskResult == nil {
		t.Fatalf("Empty create task results")
	}
	createVolumeOperationRes := createTaskResult.GetCnsVolumeOperationResult()
	if createVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to create volume: fault=%+v", createVolumeOperationRes.Fault)
	}
	volumeId := createVolumeOperationRes.VolumeId.Id
	volumeCreateResult := (createTaskResult).(*cnstypes.CnsVolumeCreateResult)
	t.Logf("volumeCreateResult %+v", volumeCreateResult)

	// Delete the static provisioning volume
	deleteVolumeList := []cnstypes.CnsVolumeId{
		{
			Id: volumeId,
		},
	}
	deleteTask, err := cnsClient.DeleteVolume(ctx, deleteVolumeList, true)

	deleteTaskInfo, err := cns.GetTaskInfo(ctx, deleteTask)
	if err != nil {
		t.Fatal(err)
	}

	deleteTaskResult, err := cns.GetTaskResult(ctx, deleteTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if deleteTaskResult == nil {
		t.Fatalf("Empty delete task results")
	}

	deleteVolumeOperationRes := deleteTaskResult.GetCnsVolumeOperationResult()
	if deleteVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to delete volume: fault=%+v", deleteVolumeOperationRes.Fault)
	}

	// Create
	createSpecList := []cnstypes.CnsVolumeCreateSpec{
		{
			Name:       "test",
			VolumeType: "TestVolumeType",
			Datastores: []vim25types.ManagedObjectReference{
				datastore.Self,
			},
			BackingObjectDetails: &cnstypes.CnsBlockBackingDetails{
				CnsBackingObjectDetails: cnstypes.CnsBackingObjectDetails{
					CapacityInMb: capacityInMb,
				},
			},
			Profile: []vim25types.BaseVirtualMachineProfileSpec{
				&vim25types.VirtualMachineDefinedProfileSpec{
					ProfileId: uuid.New().String(),
				},
			},
		},
	}
	createTask, err = cnsClient.CreateVolume(ctx, createSpecList)
	if err != nil {
		t.Fatal(err)
	}

	createTaskInfo, err = cns.GetTaskInfo(ctx, createTask)
	if err != nil {
		t.Fatal(err)
	}

	createTaskResult, err = cns.GetTaskResult(ctx, createTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if createTaskResult == nil {
		t.Fatalf("Empty create task results")
	}
	createVolumeOperationRes = createTaskResult.GetCnsVolumeOperationResult()
	if createVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to create volume: fault=%+v", createVolumeOperationRes.Fault)
	}
	volumeId = createVolumeOperationRes.VolumeId.Id
	volumeCreateResult = (createTaskResult).(*cnstypes.CnsVolumeCreateResult)
	t.Logf("volumeCreateResult %+v", volumeCreateResult)

	// Extend
	var newCapacityInMb int64 = 2048
	extendSpecList := []cnstypes.CnsVolumeExtendSpec{
		{
			VolumeId:     createVolumeOperationRes.VolumeId,
			CapacityInMb: newCapacityInMb,
		},
	}
	extendTask, err := cnsClient.ExtendVolume(ctx, extendSpecList)
	if err != nil {
		t.Fatal(err)
	}

	extendTaskInfo, err := cns.GetTaskInfo(ctx, extendTask)
	if err != nil {
		t.Fatal(err)
	}

	extendTaskResult, err := cns.GetTaskResult(ctx, extendTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if extendTaskResult == nil {
		t.Fatalf("Empty extend task results")
	}

	extendVolumeOperationRes := extendTaskResult.GetCnsVolumeOperationResult()
	if extendVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to extend: fault=%+v", extendVolumeOperationRes.Fault)
	}

	// Attach
	nodeVM := model.Map().Any("VirtualMachine").(*simulator.VirtualMachine)
	attachSpecList := []cnstypes.CnsVolumeAttachDetachSpec{
		{
			VolumeId: createVolumeOperationRes.VolumeId,
			Vm:       nodeVM.Self,
		},
	}
	attachTask, err := cnsClient.AttachVolume(ctx, attachSpecList)
	if err != nil {
		t.Fatal(err)
	}

	attachTaskInfo, err := cns.GetTaskInfo(ctx, attachTask)
	if err != nil {
		t.Fatal(err)
	}

	attachTaskResult, err := cns.GetTaskResult(ctx, attachTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if attachTaskResult == nil {
		t.Fatalf("Empty attach task results")
	}

	attachVolumeOperationRes := attachTaskResult.GetCnsVolumeOperationResult()
	if attachVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to attach: fault=%+v", attachVolumeOperationRes.Fault)
	}

	// Detach
	detachVolumeList := []cnstypes.CnsVolumeAttachDetachSpec{
		{
			VolumeId: createVolumeOperationRes.VolumeId,
		},
	}
	detachTask, err := cnsClient.DetachVolume(ctx, detachVolumeList)

	detachTaskInfo, err := cns.GetTaskInfo(ctx, detachTask)
	if err != nil {
		t.Fatal(err)
	}

	detachTaskResult, err := cns.GetTaskResult(ctx, detachTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if detachTaskResult == nil {
		t.Fatalf("Empty detach task results")
	}

	detachVolumeOperationRes := detachTaskResult.GetCnsVolumeOperationResult()
	if detachVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to detach volume: fault=%+v", detachVolumeOperationRes.Fault)
	}
	for _, d := range nodeVM.Config.Hardware.Device {
		if disk, ok := d.(*vim25types.VirtualDisk); ok && disk.VDiskId != nil && disk.VDiskId.Id == createVolumeOperationRes.VolumeId.Id {
			t.Fatal("detach with no Vm left the device")
		}
	}

	// Query
	queryFilter = cnstypes.CnsQueryFilter{}
	queryResult, err = cnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		t.Fatal(err)
	}

	if len(queryResult.Volumes) != existingNumDisks+1 {
		t.Fatal("Number of volumes mismatches after creating a single volume")
	}

	// QueryAsync
	queryFilter = cnstypes.CnsQueryFilter{}
	querySelection := cnstypes.CnsQuerySelection{}
	queryVolumeAsyncTask, err := cnsClient.QueryVolumeAsync(ctx, queryFilter, &querySelection)
	if err != nil {
		t.Fatal(err)
	}
	queryVolumeAsyncTaskInfo, err := cns.GetTaskInfo(ctx, queryVolumeAsyncTask)
	if err != nil {
		t.Fatal(err)
	}
	queryVolumeAsyncTaskResult, err := cns.GetTaskResult(ctx, queryVolumeAsyncTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if queryVolumeAsyncTaskResult == nil {
		t.Fatalf("Empty queryVolumeAsync Task Result")
	}
	queryVolumeAsyncOperationRes := queryVolumeAsyncTaskResult.GetCnsVolumeOperationResult()
	if queryVolumeAsyncOperationRes.Fault != nil {
		t.Fatalf("Failed to query volume detail using queryVolumeAsync: fault=%+v", queryVolumeAsyncOperationRes.Fault)
	}

	// QueryVolume
	queryFilter = cnstypes.CnsQueryFilter{}
	queryResult, err = cnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		t.Fatalf("Failed to query volume with QueryFilter: err=%+v", err)
	}
	if len(queryResult.Volumes) != existingNumDisks+1 {
		t.Fatal("Number of volumes mismatches after deleting a single volume")
	}
	// QueryVolumeInfo
	queryVolumeInfoTask, err := cnsClient.QueryVolumeInfo(ctx, []cnstypes.CnsVolumeId{{Id: createVolumeOperationRes.VolumeId.Id}})
	if err != nil {
		t.Fatal(err)
	}
	queryVolumeInfoTaskInfo, err := cns.GetTaskInfo(ctx, queryVolumeInfoTask)
	if err != nil {
		t.Fatal(err)
	}
	queryVolumeInfoTaskResult, err := cns.GetTaskResult(ctx, queryVolumeInfoTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if queryVolumeInfoTaskResult == nil {
		t.Fatalf("Empty query VolumeInfo Task Result")
	}
	queryVolumeInfoOperationRes := queryVolumeInfoTaskResult.GetCnsVolumeOperationResult()
	if queryVolumeInfoOperationRes.Fault != nil {
		t.Fatalf("Failed to query volume detail using QueryVolumeInfo: fault=%+v", queryVolumeInfoOperationRes.Fault)
	}

	// QueryAll
	queryFilter = cnstypes.CnsQueryFilter{}
	querySelection = cnstypes.CnsQuerySelection{}
	queryResult, err = cnsClient.QueryAllVolume(ctx, queryFilter, querySelection)

	if len(queryResult.Volumes) != existingNumDisks+1 {
		t.Fatal("Number of volumes mismatches after creating a single volume")
	}

	// Update
	var metadataList []cnstypes.BaseCnsEntityMetadata
	newLabels := []vim25types.KeyValue{
		{
			Key:   testLabel,
			Value: testValue,
		},
	}
	metadata := &cnstypes.CnsKubernetesEntityMetadata{

		CnsEntityMetadata: cnstypes.CnsEntityMetadata{
			DynamicData: vim25types.DynamicData{},
			EntityName:  queryResult.Volumes[0].Name,
			Labels:      newLabels,
			Delete:      false,
		},
		EntityType: string(cnstypes.CnsKubernetesEntityTypePV),
		Namespace:  "",
	}
	metadataList = append(metadataList, cnstypes.BaseCnsEntityMetadata(metadata))
	updateSpecList := []cnstypes.CnsVolumeMetadataUpdateSpec{
		{
			DynamicData: vim25types.DynamicData{},
			VolumeId:    createVolumeOperationRes.VolumeId,
			Metadata: cnstypes.CnsVolumeMetadata{
				DynamicData:      vim25types.DynamicData{},
				ContainerCluster: queryResult.Volumes[0].Metadata.ContainerCluster,
				EntityMetadata:   metadataList,
			},
		},
	}
	updateTask, err := cnsClient.UpdateVolumeMetadata(ctx, updateSpecList)
	if err != nil {
		t.Fatal(err)
	}
	updateTaskInfo, err := cns.GetTaskInfo(ctx, updateTask)
	if err != nil {
		t.Fatal(err)
	}
	updateTaskResult, err := cns.GetTaskResult(ctx, updateTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if updateTaskResult == nil {
		t.Fatalf("Empty create task results")
	}

	updateVolumeOperationRes := updateTaskResult.GetCnsVolumeOperationResult()
	if updateVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to create volume: fault=%+v", updateVolumeOperationRes.Fault)
	}

	// Create Snapshots
	desc := "simulator-snapshot"
	var cnsSnapshotCreateSpecList []cnstypes.CnsSnapshotCreateSpec
	cnsSnapshotCreateSpec := cnstypes.CnsSnapshotCreateSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: volumeId,
		},
		Description: desc,
	}
	cnsSnapshotCreateSpecList = append(cnsSnapshotCreateSpecList, cnsSnapshotCreateSpec)
	createSnapshotsTask, err := cnsClient.CreateSnapshots(ctx, cnsSnapshotCreateSpecList)
	if err != nil {
		t.Fatalf("Failed to get the task of CreateSnapshots. Error: %+v \n", err)
	}
	createSnapshotsTaskInfo, err := cns.GetTaskInfo(ctx, createSnapshotsTask)
	if err != nil {
		t.Fatalf("Failed to get the task info of CreateSnapshots. Error: %+v \n", err)
	}
	createSnapshotsTaskResult, err := cns.GetTaskResult(ctx, createSnapshotsTaskInfo)
	if err != nil {
		t.Fatalf("Failed to get the task result of CreateSnapshots. Error: %+v \n", err)
	}
	createSnapshotsOperationRes := createSnapshotsTaskResult.GetCnsVolumeOperationResult()
	if createSnapshotsOperationRes.Fault != nil {
		t.Fatalf("Failed to create snapshots: fault=%+v", createSnapshotsOperationRes.Fault)
	}

	snapshotCreateResult := any(createSnapshotsTaskResult).(*cnstypes.CnsSnapshotCreateResult)
	snapshotId := snapshotCreateResult.Snapshot.SnapshotId.Id
	t.Logf("snapshotCreateResult %+v", snapshotCreateResult)

	// Query Snapshots
	var snapshotQueryFilter cnstypes.CnsSnapshotQueryFilter
	QuerySnapshotsFunc := func(snapshotQueryFilter cnstypes.CnsSnapshotQueryFilter) *cnstypes.CnsSnapshotQueryResult {
		querySnapshotsTask, err := cnsClient.QuerySnapshots(ctx, snapshotQueryFilter)
		if err != nil {
			t.Fatalf("Failed to get the task of QuerySnapshots. Error: %+v \n", err)
		}
		querySnapshotsTaskInfo, err := cns.GetTaskInfo(ctx, querySnapshotsTask)
		if err != nil {
			t.Fatalf("Failed to get the task info of QuerySnapshots. Error: %+v \n", err)
		}
		querySnapshotsTaskResult, err := cns.GetQuerySnapshotsTaskResult(ctx, querySnapshotsTaskInfo)
		if err != nil {
			t.Fatalf("Failed to get the task result of QuerySnapshots. Error: %+v \n", err)
		}
		return querySnapshotsTaskResult
	}

	// snapshot query filter 1: empty snapshot query spec => return all snapshots known to CNS
	snapshotQueryFilter = cnstypes.CnsSnapshotQueryFilter{
		SnapshotQuerySpecs: nil,
	}
	t.Logf("snapshotQueryFilter with empty SnapshotQuerySpecs: %+v", snapshotQueryFilter)
	querySnapshotsTaskResult := QuerySnapshotsFunc(snapshotQueryFilter)
	t.Logf("snapshotQueryResult %+v", querySnapshotsTaskResult)

	// snapshot query filter 2: unknown volumeId
	snapshotQueryFilter = cnstypes.CnsSnapshotQueryFilter{
		SnapshotQuerySpecs: []cnstypes.CnsSnapshotQuerySpec{
			{
				VolumeId: cnstypes.CnsVolumeId{Id: "unknown-volume-id"},
			},
		},
	}
	t.Logf("snapshotQueryFilter with unknown volumeId in SnapshotQuerySpecs: %+v", snapshotQueryFilter)
	querySnapshotsTaskResult = QuerySnapshotsFunc(snapshotQueryFilter)
	_, ok := querySnapshotsTaskResult.Entries[0].Error.Fault.(*cnstypes.CnsVolumeNotFoundFault)
	if !ok {
		t.Fatalf("Unexpected error returned while CnsVolumeNotFoundFault is expected. Error: %+v \n", querySnapshotsTaskResult.Entries[0].Error.Fault)
	}
	t.Logf("snapshotQueryResult with expected CnsVolumeNotFoundFault %+v", querySnapshotsTaskResult)

	// snapshot query filter 3: unknown snapshotId
	snapshotQueryFilter = cnstypes.CnsSnapshotQueryFilter{
		SnapshotQuerySpecs: []cnstypes.CnsSnapshotQuerySpec{
			{
				VolumeId:   cnstypes.CnsVolumeId{Id: volumeId},
				SnapshotId: &cnstypes.CnsSnapshotId{Id: "unknown-snapshot-id"},
			},
		},
	}
	t.Logf("snapshotQueryFilter with unknown snapshotId in SnapshotQuerySpecs: %+v", snapshotQueryFilter)
	querySnapshotsTaskResult = QuerySnapshotsFunc(snapshotQueryFilter)
	_, ok = querySnapshotsTaskResult.Entries[0].Error.Fault.(*cnstypes.CnsSnapshotNotFoundFault)
	if !ok {
		t.Fatalf("Unexpected error returned while CnsSnapshotNotFoundFault is expected. Error: %+v \n", querySnapshotsTaskResult.Entries[0].Error.Fault)
	}
	t.Logf("snapshotQueryResult with expected CnsSnapshotNotFoundFault %+v", querySnapshotsTaskResult)

	// snapshot query filter 4: expected volumeId
	snapshotQueryFilter = cnstypes.CnsSnapshotQueryFilter{
		SnapshotQuerySpecs: []cnstypes.CnsSnapshotQuerySpec{
			{
				VolumeId: cnstypes.CnsVolumeId{Id: volumeId},
			},
		},
	}
	t.Logf("snapshotQueryFilter with expected volumeId and empty snapshotId in SnapshotQuerySpecs: %+v", snapshotQueryFilter)
	querySnapshotsTaskResult = QuerySnapshotsFunc(snapshotQueryFilter)
	t.Logf("snapshotQueryResult %+v", querySnapshotsTaskResult)

	// snapshot query filter 5: expected volumeId and snapshotId
	snapshotQueryFilter = cnstypes.CnsSnapshotQueryFilter{
		SnapshotQuerySpecs: []cnstypes.CnsSnapshotQuerySpec{
			{
				VolumeId:   cnstypes.CnsVolumeId{Id: volumeId},
				SnapshotId: &cnstypes.CnsSnapshotId{Id: snapshotId},
			},
		},
	}
	t.Logf("snapshotQueryFilter with expected volumeId and snapshotId in SnapshotQuerySpecs: %+v", snapshotQueryFilter)
	querySnapshotsTaskResult = QuerySnapshotsFunc(snapshotQueryFilter)
	t.Logf("snapshotQueryResult %+v", querySnapshotsTaskResult)

	// Delete Snapshots
	var cnsSnapshotDeleteSpecList []cnstypes.CnsSnapshotDeleteSpec
	cnsSnapshotDeleteSpec := cnstypes.CnsSnapshotDeleteSpec{
		VolumeId: cnstypes.CnsVolumeId{
			Id: volumeId,
		},
		SnapshotId: cnstypes.CnsSnapshotId{
			Id: snapshotId,
		},
	}
	cnsSnapshotDeleteSpecList = append(cnsSnapshotDeleteSpecList, cnsSnapshotDeleteSpec)
	deleteSnapshotsTask, err := cnsClient.DeleteSnapshots(ctx, cnsSnapshotDeleteSpecList)
	if err != nil {
		t.Fatalf("Failed to get the task of DeleteSnapshots. Error: %+v \n", err)
	}
	deleteSnapshotsTaskInfo, err := cns.GetTaskInfo(ctx, deleteSnapshotsTask)
	if err != nil {
		t.Fatalf("Failed to get the task info of DeleteSnapshots. Error: %+v \n", err)
	}

	deleteSnapshotsTaskResult, err := cns.GetTaskResult(ctx, deleteSnapshotsTaskInfo)
	if err != nil {
		t.Fatalf("Failed to get the task result of DeleteSnapshots. Error: %+v \n", err)
	}

	deleteSnapshotsOperationRes := deleteSnapshotsTaskResult.GetCnsVolumeOperationResult()
	if deleteSnapshotsOperationRes.Fault != nil {
		t.Fatalf("Failed to delete snapshots: fault=%+v", deleteSnapshotsOperationRes.Fault)
	}

	snapshotDeleteResult := any(deleteSnapshotsTaskResult).(*cnstypes.CnsSnapshotDeleteResult)
	t.Logf("snapshotDeleteResult %+v", snapshotDeleteResult)

	// Delete
	deleteVolumeList = []cnstypes.CnsVolumeId{
		{
			Id: volumeId,
		},
	}
	deleteTask, err = cnsClient.DeleteVolume(ctx, deleteVolumeList, true)

	deleteTaskInfo, err = cns.GetTaskInfo(ctx, deleteTask)
	if err != nil {
		t.Fatal(err)
	}

	deleteTaskResult, err = cns.GetTaskResult(ctx, deleteTaskInfo)
	if err != nil {
		t.Fatal(err)
	}
	if deleteTaskResult == nil {
		t.Fatalf("Empty delete task results")
	}

	deleteVolumeOperationRes = deleteTaskResult.GetCnsVolumeOperationResult()
	if deleteVolumeOperationRes.Fault != nil {
		t.Fatalf("Failed to delete volume: fault=%+v", deleteVolumeOperationRes.Fault)
	}

	queryFilter = cnstypes.CnsQueryFilter{}
	queryResult, err = cnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		t.Fatalf("Failed to query volume with QueryFilter: err=%+v", err)
	}
	if len(queryResult.Volumes) != existingNumDisks {
		t.Fatal("Number of volumes mismatches after deleting a single volume")
	}

}

// attachEnv drives CNS attach and detach against a VPX model.
type attachEnv struct {
	t     *testing.T
	ctx   context.Context
	model *simulator.Model
	cns   *cns.Client
	vms   []*simulator.VirtualMachine
}

func testAttach(t *testing.T, f func(*attachEnv)) {
	t.Helper()
	ctx := context.Background()
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	s := model.Service.NewServer()
	defer s.Close()
	model.Service.RegisterSDK(New())

	c, err := govmomi.NewClient(ctx, s.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	cnsClient, err := cns.NewClient(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}

	e := &attachEnv{t: t, ctx: ctx, model: model, cns: cnsClient}
	for _, ref := range model.Map().All("VirtualMachine") {
		e.vms = append(e.vms, ref.(*simulator.VirtualMachine))
	}
	slices.SortFunc(e.vms, func(a, b *simulator.VirtualMachine) int { return strings.Compare(a.Name, b.Name) })
	f(e)
}

func (e *attachEnv) createVolume(name string) cnstypes.CnsVolumeId {
	e.t.Helper()
	ds := e.model.Map().Any("Datastore").(*simulator.Datastore)
	res := e.results(e.cns.CreateVolume(e.ctx, []cnstypes.CnsVolumeCreateSpec{{
		Name:       name,
		VolumeType: "BLOCK",
		Datastores: []vim25types.ManagedObjectReference{ds.Self},
		BackingObjectDetails: &cnstypes.CnsBlockBackingDetails{
			CnsBackingObjectDetails: cnstypes.CnsBackingObjectDetails{CapacityInMb: 10},
		},
	}}))
	if res[0].GetCnsVolumeOperationResult().Fault != nil {
		e.t.Fatal(res[0].GetCnsVolumeOperationResult().Fault)
	}
	return res[0].GetCnsVolumeOperationResult().VolumeId
}

// results waits for a CNS task, which must succeed, and returns its per-volume results.
func (e *attachEnv) results(t *object.Task, err error) []cnstypes.BaseCnsVolumeOperationResult {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := cns.GetTaskInfo(e.ctx, t)
	if err != nil {
		e.t.Fatal(err)
	}
	batch, ok := info.Result.(cnstypes.CnsVolumeOperationBatchResult)
	if !ok {
		e.t.Fatalf("result %T", info.Result)
	}
	return batch.VolumeResults
}

func (e *attachEnv) attach(specs ...cnstypes.CnsVolumeAttachDetachSpec) []cnstypes.BaseCnsVolumeOperationResult {
	e.t.Helper()
	return e.results(e.cns.AttachVolume(e.ctx, specs))
}

func (e *attachEnv) detach(specs ...cnstypes.CnsVolumeAttachDetachSpec) []cnstypes.BaseCnsVolumeOperationResult {
	e.t.Helper()
	return e.results(e.cns.DetachVolume(e.ctx, specs))
}

func spec(id cnstypes.CnsVolumeId, vm *simulator.VirtualMachine) cnstypes.CnsVolumeAttachDetachSpec {
	return cnstypes.CnsVolumeAttachDetachSpec{VolumeId: id, Vm: vm.Self}
}

func diskOf(vm *simulator.VirtualMachine, id cnstypes.CnsVolumeId) *vim25types.VirtualDisk {
	for _, d := range vm.Config.Hardware.Device {
		if disk, ok := d.(*vim25types.VirtualDisk); ok && disk.VDiskId != nil && disk.VDiskId.Id == id.Id {
			return disk
		}
	}
	return nil
}

func diskCount(vm *simulator.VirtualMachine) int {
	return len(object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*vim25types.VirtualDisk)(nil)))
}

func requireNoFault(t *testing.T, res cnstypes.BaseCnsVolumeOperationResult) {
	t.Helper()
	if f := res.GetCnsVolumeOperationResult().Fault; f != nil {
		t.Fatalf("fault %T", f.Fault)
	}
}

func requireVolumeFault[T vim25types.BaseMethodFault](t *testing.T, res cnstypes.BaseCnsVolumeOperationResult) {
	t.Helper()
	f := res.GetCnsVolumeOperationResult().Fault
	if f == nil {
		t.Fatal("no fault")
	}
	if _, ok := f.Fault.(T); !ok {
		var want T
		t.Fatalf("want %T, got %T", want, f.Fault)
	}
}

func diskUUID(res cnstypes.BaseCnsVolumeOperationResult) string {
	return res.(*cnstypes.CnsVolumeAttachResult).DiskUUID
}

func TestCnsAttachVolume(t *testing.T) {
	t.Run("C1 one volume", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			vm, id := e.vms[0], e.createVolume("c1")
			res := e.attach(spec(id, vm))
			requireNoFault(t, res[0])
			disk := diskOf(vm, id)
			if disk == nil || diskUUID(res[0]) != simulator.DiskUUID(disk) || diskUUID(res[0]) == "" {
				t.Fatalf("disk %v, DiskUUID %q", disk, diskUUID(res[0]))
			}
		})
	})

	t.Run("C2 two volumes in one call", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			vm, n := e.vms[0], diskCount(e.vms[0])
			res := e.attach(spec(e.createVolume("c2a"), vm), spec(e.createVolume("c2b"), vm))
			requireNoFault(t, res[0])
			requireNoFault(t, res[1])
			if diskUUID(res[0]) == diskUUID(res[1]) || diskCount(vm) != n+2 {
				t.Fatalf("DiskUUIDs %q %q, disks %d", diskUUID(res[0]), diskUUID(res[1]), diskCount(vm))
			}
		})
	})

	// C3 and C4 replace TestCnsAttachVolumeIdempotent.
	t.Run("C3 same attach twice", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			vm, id := e.vms[0], e.createVolume("c3")
			first := e.attach(spec(id, vm))
			n := diskCount(vm)
			again := e.attach(spec(id, vm))
			requireNoFault(t, again[0])
			if diskUUID(first[0]) != diskUUID(again[0]) || diskCount(vm) != n {
				t.Fatal("retry changed the VM")
			}
		})
	})

	t.Run("C4 attach to another VM", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			id := e.createVolume("c4")
			requireNoFault(t, e.attach(spec(id, e.vms[0]))[0])
			n := diskCount(e.vms[1])
			requireVolumeFault[*vim25types.ResourceInUse](t, e.attach(spec(id, e.vms[1]))[0])
			if diskCount(e.vms[1]) != n {
				t.Fatal("conflicting attach changed the other VM")
			}
		})
	})

	t.Run("C5 free and conflicting volumes in one call", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			busy, free := e.createVolume("c5-busy"), e.createVolume("c5-free")
			requireNoFault(t, e.attach(spec(busy, e.vms[0]))[0])
			res := e.attach(spec(free, e.vms[1]), spec(busy, e.vms[1]))
			requireNoFault(t, res[0])
			if d := diskOf(e.vms[1], free); d == nil || diskUUID(res[0]) != simulator.DiskUUID(d) {
				t.Fatal("free volume not attached")
			}
			requireVolumeFault[*vim25types.ResourceInUse](t, res[1])
		})
	})

	t.Run("C6 placement and modes from the spec", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			vm, id := e.vms[0], e.createVolume("c6")
			controller := diskOfAny(vm).ControllerKey
			s := spec(id, vm)
			s.ControllerKey, s.UnitNumber = &controller, vim25types.NewInt32(5)
			s.DiskMode = string(vim25types.VirtualDiskModeIndependent_persistent)
			s.Sharing = string(vim25types.VirtualDiskSharingSharingNone)
			requireNoFault(t, e.attach(s)[0])
			d := diskOf(vm, id)
			b := d.Backing.(*vim25types.VirtualDiskFlatVer2BackingInfo)
			if d.ControllerKey != controller || *d.UnitNumber != 5 || b.DiskMode != s.DiskMode || b.Sharing != s.Sharing {
				t.Fatalf("device %d:%d %s %s", d.ControllerKey, *d.UnitNumber, b.DiskMode, b.Sharing)
			}
		})
	})

	t.Run("C7 unknown VM and volume", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			id := e.createVolume("c7")
			res := e.attach(cnstypes.CnsVolumeAttachDetachSpec{
				VolumeId: id,
				Vm:       vim25types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-unknown"},
			}, spec(cnstypes.CnsVolumeId{Id: "unknown"}, e.vms[0]))
			requireVolumeFault[*vim25types.ManagedObjectNotFound](t, res[0])
			requireVolumeFault[*vim25types.NotFound](t, res[1])
		})
	})
}

func TestCnsDetachVolume(t *testing.T) {
	t.Run("C8 detach then attach elsewhere", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			id := e.createVolume("c8")
			requireNoFault(t, e.attach(spec(id, e.vms[0]))[0])
			requireNoFault(t, e.detach(spec(id, e.vms[0]))[0])
			if diskOf(e.vms[0], id) != nil {
				t.Fatal("device remains")
			}
			requireNoFault(t, e.attach(spec(id, e.vms[1]))[0])
		})
	})

	t.Run("C9 detach of an unheld volume", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			requireVolumeFault[*vim25types.NotFound](t, e.detach(spec(e.createVolume("c9"), e.vms[0]))[0])
		})
	})

	t.Run("C11 delete while attached", func(t *testing.T) {
		testAttach(t, func(e *attachEnv) {
			id := e.createVolume("c11")
			requireNoFault(t, e.attach(spec(id, e.vms[0]))[0])
			del := func() []cnstypes.BaseCnsVolumeOperationResult {
				return e.results(e.cns.DeleteVolume(e.ctx, []cnstypes.CnsVolumeId{id}, true))
			}
			requireVolumeFault[*vim25types.InvalidState](t, del()[0])
			q, err := e.cns.QueryVolume(e.ctx, &cnstypes.CnsQueryFilter{VolumeIds: []cnstypes.CnsVolumeId{id}})
			if err != nil || len(q.Volumes) != 1 {
				t.Fatalf("volume gone after refused delete: %v", err)
			}
			requireNoFault(t, e.detach(spec(id, e.vms[0]))[0])
			requireNoFault(t, del()[0])
		})
	})
}

func diskOfAny(vm *simulator.VirtualMachine) *vim25types.VirtualDisk {
	return object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*vim25types.VirtualDisk)(nil))[0].(*vim25types.VirtualDisk)
}
