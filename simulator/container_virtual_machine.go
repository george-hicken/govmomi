// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"archive/tar"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

// ContainerBackingOptionKey is the ExtraConfig key that backs a VM with a
// container. Its value is an image name, or a JSON array of an image name and
// the command to run in it, such as ["alpine", "sleep", "infinity"]. The
// container is created at first power-on.
//
// These ExtraConfig keys configure the container. Boolean values are JSON,
// "true" or "false"; any other value leaves the default.
//
//   - RUN.mountdmi (default true): mount the VM's DMI data at /sys/class/dmi/id.
//     Set false where that mount is refused, as under rootless podman.
//   - RUN.machineId (default true): mount a file holding config.uuid, the
//     VM's BIOS UUID, as 32 lowercase hex digits read-only at /etc/machine-id
//     and /var/lib/dbus/machine-id, except where a RUN.volume targets that
//     path. A guest that has no DMI data, such as one under rootless podman,
//     then still identifies itself by its BIOS UUID, as the kubelet does for
//     its node's systemUUID, and no two VMs share the machine ID baked into
//     an image.
//   - RUN.network: the container network to join, such as "podman".
//   - RUN.privileged (default false): run the container with --privileged, as
//     an image with systemd as PID 1 needs.
//   - RUN.nestedContainers (default false): let the container run containers,
//     for example a Kubernetes node. It adds --privileged, --cgroupns=private,
//     unconfined seccomp and apparmor, tmpfs at /tmp and /run, a volume at
//     /var, /lib/modules read-only and /dev/fuse, adapted from kind. Without it
//     the container shares the host's cgroup namespace.
//   - RUN.vmci (default false): serve GuestRPC to the guest, described below.
//   - RUN.port.<port>: publish container port <port> on the host port given as
//     the value.
//   - RUN.volume.<label>: add a mount, "source:destination[:options]" as for
//     docker -v, where source is a host path or a volume name.
//   - RUN.env.<name>: set environment variable <name> to the value.
//   - guestinfo.<key>: when the container is created, each becomes the
//     environment variable VMX_GUESTINFO_<KEY>, and VMX_GUESTINFO=true is set.
//     In a container this is the only guestinfo source cloud-init's VMware
//     datasource reads, because it detects the container and skips
//     vmware-rpctool.
//
// Each virtual Ethernet card is an interface of the container, eth<i> in
// device-key order, with the card's MAC, as a guest on a real VM sees it, so a
// guest that configures its network by that MAC, as cloud-init does, finds its
// NIC. With a topology loaded each card joins the podman network of its L2
// domain; see containerNICs and networkArgs. Without one, the container gets the
// first card's MAC on the runtime's default network.
//
// With RUN.vmci=true the guest can also read and write guestinfo while it runs.
// vcsim serves GuestRPC on a Unix socket mounted at /run/vmware/rpc.sock, names
// it in $VMX_RPC_SOCK, and mounts the govmomi toolbox binary at
// /usr/bin/vmware-rpctool unless a RUN.volume already targets that path; see
// VmciToolboxBinaryPath to mount it as vmtoolsd as well. The guest may read
// guestinfo.* keys and write those without "/". A write changes
// config.extraConfig at once and is reported by PropertyCollector.
//
// Not simulated:
//
//   - AF_VSOCK and the x86 backdoor. Neither exists in the container, so stock
//     open-vm-tools vmtoolsd and vmware-rpctool cannot reach vcsim; only the
//     govmomi toolbox binary can.
//   - Host-to-guest GuestRPC (TCLO). There are no tools-mediated power
//     operations. Guest operations are served with docker exec, not through
//     tools.
//
// With RUN.vmci=true, guest.toolsRunningStatus and the tools version follow
// what the guest reports over GuestRPC, as they do on a real VM: the govmomi
// toolbox daemon is running while its connection is open, and vmtoolsd while
// its TCLO heartbeat continues (see guestTools). The guest also reports its
// own network identity, which then replaces the container's observed address.
// Without RUN.vmci there is no tools channel, and guest.toolsRunningStatus
// follows the container's state.
//
// For example, a Kubernetes node that reports its status through guestinfo:
//
//	ExtraConfig: []types.BaseOptionValue{
//		&types.OptionValue{Key: "RUN.container", Value: "my-k8s-node:latest"},
//		&types.OptionValue{Key: "RUN.nestedContainers", Value: "true"},
//		&types.OptionValue{Key: "RUN.vmci", Value: "true"},
//		&types.OptionValue{Key: "guestinfo.userdata", Value: userdata},
//	}
const ContainerBackingOptionKey = "RUN.container"

var (
	// toolsRunning and toolsNotRunning leave the tools version in place, as
	// vCenter keeps reporting the installed tools of a powered-off VM.
	toolsRunning = []types.PropertyChange{
		{Name: "guest.toolsStatus", Val: types.VirtualMachineToolsStatusToolsOk},
		{Name: "guest.toolsRunningStatus", Val: string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)},
		{Name: "summary.guest.toolsStatus", Val: types.VirtualMachineToolsStatusToolsOk},
		{Name: "summary.guest.toolsRunningStatus", Val: string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)},
	}

	toolsNotRunning = []types.PropertyChange{
		{Name: "guest.toolsStatus", Val: types.VirtualMachineToolsStatusToolsNotRunning},
		{Name: "guest.toolsRunningStatus", Val: string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)},
		{Name: "summary.guest.toolsStatus", Val: types.VirtualMachineToolsStatusToolsNotRunning},
		{Name: "summary.guest.toolsRunningStatus", Val: string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)},
	}
)

type simVM struct {
	vm *VirtualMachine
	c  *container

	// guestRPCDir is the host directory mounted at /run/vmware when RUN.vmci
	// is true. It lives as long as the container, so a restarted container
	// finds its mount source; guestRPC serves the socket in it while the VM is
	// powered on.
	guestRPCDir string
	guestRPC    *guestRPCServer

	// machineIDDir is the host directory holding the machine-id file mounted
	// at machineIDPaths when RUN.machineId is true. Like guestRPCDir it lives
	// as long as the container.
	machineIDDir string

	// toolsNicInfo is set while the guest's tools have reported its NIC info
	// in this power-on, which then takes precedence over the container's
	// observed network settings (spec 95 D-95-31).
	toolsNicInfo atomic.Bool
}

// createSimulationVM inspects the provided VirtualMachine and creates a simVM binding for it if
// the vm.Config.ExtraConfig set contains a key "RUN.container".
// If the ExtraConfig set does not contain that key, this returns nil.
// Methods on the simVM type are written to check for nil object so the return from this call can be blindly
// assigned and invoked without the caller caring about whether a binding for a backing container was warranted.
func createSimulationVM(vm *VirtualMachine) *simVM {
	svm := &simVM{
		vm: vm,
	}

	for _, opt := range vm.Config.ExtraConfig {
		val := opt.GetOptionValue()
		if val.Key == ContainerBackingOptionKey {
			return svm
		}
	}

	return nil
}

// syncNetworkConfigToVMGuestProperties applies container network settings to
// vm.Guest properties and triggers granular property-change notifications.
// Once the guest has reported its NIC info over GuestRPC, only the power
// state is applied.
//
// out and detail are an optional docker/podman inspect result (e.g.
// from watchContainer's update loop, which must inspect anyway to detect
// container removal). If detail is nil this function perform its own inspect().
//
// The inspect() call, when performed here, happens outside the lock to avoid
// holding it during I/O. ctx.AutoUpdate acquires the VM lock once for the
// whole checkpoint → modify → diff → update sequence, preventing races with
// concurrent SOAP handlers (e.g. DestroyTask) that also hold the VM lock.
func (svm *simVM) syncNetworkConfigToVMGuestProperties(ctx *Context, out []byte, detail *containerDetails) error {
	if svm == nil || ctx == nil {
		return nil
	}

	if detail == nil {
		o, d, err := svm.c.inspect()
		if err != nil {
			return err
		}
		out = o
		detail = &d
	}

	// Precompute values that don't need the VM lock.
	//
	// primaryNet is chosen by taking an arbitrary entry from Go's randomized
	// map iteration over multi-network containers. This is intentional instability
	// until we've verified the real VC algo for selecting "primary" network.
	primaryNet := detail.NetworkSettings.networkSettings
	for _, n := range detail.NetworkSettings.Networks {
		primaryNet = n
		break
	}

	ctx.AutoUpdate(svm.vm, func() {
		svm.vm.Config.Annotation = "inspect"
		svm.vm.logPrintf("%s: %s", svm.vm.Config.Annotation, string(out))

		live := ctx.Map.underlay != nil
		var topoNICs []types.GuestNicInfo
		if live {
			topoNICs, primaryNet = svm.nicGuestInfo(ctx, detail.NetworkSettings.Networks)
		}

		if detail.State.Paused {
			svm.vm.Runtime.PowerState = types.VirtualMachinePowerStateSuspended
		} else if detail.State.Running {
			svm.vm.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOn
		} else {
			svm.vm.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff
		}

		// The container's network settings are the address observed from
		// outside the guest, reported only while the guest's own tools have
		// not reported its NICs (spec 95 D-95-31).
		if svm.toolsNicInfo.Load() {
			return
		}

		svm.vm.Guest.IpAddress = primaryNet.IPAddress
		svm.vm.Summary.Guest.IpAddress = primaryNet.IPAddress

		if detail.Config.Hostname != "" {
			svm.vm.Guest.HostName = detail.Config.Hostname
			svm.vm.Summary.Guest.HostName = detail.Config.Hostname
		}

		// Without a topology the container's networks are the guest's NICs. With one they are matched to the
		// VM's vNICs by MAC, so each NIC is reported whether or not it has an address.
		var guestNics []types.GuestNicInfo
		if live {
			guestNics = topoNICs
		} else {
			nicIndex := int32(0)
			for networkName, netSettings := range detail.NetworkSettings.Networks {
				if netSettings.IPAddress == "" {
					continue
				}
				nic := types.GuestNicInfo{
					Network:        networkName,
					IpAddress:      []string{netSettings.IPAddress},
					MacAddress:     netSettings.MacAddress,
					Connected:      true,
					DeviceConfigId: nicIndex,
					IpConfig: &types.NetIpConfigInfo{
						IpAddress: []types.NetIpConfigInfoIpAddress{{
							IpAddress:    netSettings.IPAddress,
							PrefixLength: int32(netSettings.IPPrefixLen),
							State:        string(types.NetIpConfigInfoIpAddressStatusPreferred),
						}},
					},
				}
				guestNics = append(guestNics, nic)
				nicIndex++
			}

			if len(guestNics) == 0 && primaryNet.IPAddress != "" {
				guestNics = append(guestNics, types.GuestNicInfo{
					Network:        "default",
					IpAddress:      []string{primaryNet.IPAddress},
					MacAddress:     primaryNet.MacAddress,
					Connected:      true,
					DeviceConfigId: 0,
					IpConfig: &types.NetIpConfigInfo{
						IpAddress: []types.NetIpConfigInfoIpAddress{{
							IpAddress:    primaryNet.IPAddress,
							PrefixLength: int32(primaryNet.IPPrefixLen),
							State:        string(types.NetIpConfigInfoIpAddressStatusPreferred),
						}},
					},
				})
			}
		}

		svm.vm.Guest.Net = guestNics

		if primaryNet.IPAddress != "" {
			svm.vm.Guest.IpStack = []types.GuestStackInfo{{
				DnsConfig: &types.NetDnsConfigInfo{
					Dhcp:         false,
					HostName:     svm.vm.Guest.HostName,
					DomainName:   detail.Config.Domainname,
					IpAddress:    detail.Config.DNS,
					SearchDomain: nil,
				},
				IpRouteConfig: &types.NetIpRouteConfigInfo{
					IpRoute: []types.NetIpRouteConfigInfoIpRoute{{
						Network:      "0.0.0.0",
						PrefixLength: 0,
						Gateway: types.NetIpRouteConfigInfoGateway{
							IpAddress: primaryNet.Gateway,
							Device:    "0",
						},
					}},
				},
			}}
		}

		// The vNIC's MAC is the input the container is attached with, so only the legacy path reports the
		// container's MAC back onto the first card.
		if !live {
			for _, d := range svm.vm.Config.Hardware.Device {
				if eth, ok := d.(types.BaseVirtualEthernetCard); ok {
					eth.GetVirtualEthernetCard().MacAddress = primaryNet.MacAddress
					break
				}
			}
		}
	})

	return nil
}

func (svm *simVM) prepareGuestOperation(auth types.BaseGuestAuthentication) types.BaseMethodFault {
	if svm == nil || svm.c == nil || svm.c.id == "" {
		return new(types.GuestOperationsUnavailable)
	}

	if svm.vm.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
		return &types.InvalidPowerState{
			RequestedState: types.VirtualMachinePowerStatePoweredOn,
			ExistingState:  svm.vm.Runtime.PowerState,
		}
	}

	switch creds := auth.(type) {
	case *types.NamePasswordAuthentication:
		if creds.Username == "" || creds.Password == "" {
			return new(types.InvalidGuestLogin)
		}
	default:
		return new(types.InvalidGuestLogin)
	}

	return nil
}

// biosUUID returns the VM's BIOS UUID, config.uuid, which a client may set
// with spec.uuid. It falls back to the UUID vcsim derived for the VM when
// config.uuid does not parse.
func (svm *simVM) biosUUID() uuid.UUID {
	if id, err := uuid.Parse(svm.vm.Config.Uuid); err == nil {
		return id
	}
	return svm.vm.uid
}

// populateDMI writes BIOS UUID DMI files to a container volume
func (svm *simVM) populateDMI() error {
	if svm.c == nil {
		return nil
	}

	id := svm.biosUUID()
	files := []tarEntry{
		{
			&tar.Header{
				Name: "product_uuid",
				Mode: 0444,
			},
			[]byte(productUUID(id)),
		},
		{
			&tar.Header{
				Name: "product_serial",
				Mode: 0444,
			},
			[]byte(productSerial(id)),
		},
	}

	_, err := svm.c.createVolume("dmi", []string{deleteWithContainer}, files)
	return err
}

// start runs the container if specified by the RUN.container extraConfig property.
// lazily creates a container backing if specified by an ExtraConfig property with key "RUN.container"
func (svm *simVM) start(ctx *Context) error {
	if svm == nil {
		return nil
	}

	if svm.c != nil && svm.c.id != "" {
		// config.uuid may have been reconfigured while the VM was off.
		if err := svm.writeMachineID(); err != nil {
			return err
		}

		if err := svm.startGuestRPC(ctx); err != nil {
			return err
		}

		err := svm.c.start(ctx)
		if err != nil {
			log.Printf("%s %s: %s", svm.vm.Name, "start", err)
			svm.stopGuestRPC()
		} else {
			svm.updateToolsStatus(ctx, false)
		}

		return err
	}

	var args []string
	var env []string
	var ports []string
	var networks []string
	var extraVolumes []string
	jsonArgs := false
	mountDMI := true
	mountMachineID := true
	privileged := false
	nestedContainers := false
	vmci := false

	// The keys are documented on ContainerBackingOptionKey.
	for _, opt := range svm.vm.Config.ExtraConfig {
		val := opt.GetOptionValue()
		// Any value is accepted as its string form: a client may send one
		// typed other than xsd:string, and a failed type assertion here would
		// panic the power-on task.
		value := fmt.Sprint(val.Value)

		if val.Key == ContainerBackingOptionKey {
			run := value
			if err := json.Unmarshal([]byte(run), &args); err != nil {
				args = []string{run}
			} else {
				jsonArgs = true
			}

			continue
		}

		if val.Key == "RUN.mountdmi" {
			parseBoolOption(value, &mountDMI)
			continue
		}

		if val.Key == "RUN.machineId" {
			parseBoolOption(value, &mountMachineID)
			continue
		}

		if val.Key == "RUN.network" {
			// Specify container network (e.g., "podman" for rootless podman bridge network)
			// This is important for rootless podman which doesn't assign IPs without a network
			networks = append(networks, value)
			continue
		}

		if val.Key == "RUN.privileged" {
			parseBoolOption(value, &privileged)
			continue
		}

		if val.Key == "RUN.nestedContainers" {
			parseBoolOption(value, &nestedContainers)
			continue
		}

		if val.Key == "RUN.vmci" {
			parseBoolOption(value, &vmci)
			continue
		}

		if strings.HasPrefix(val.Key, "RUN.port.") {
			// ? would this not make more sense as a set of tuples in the value?
			// or inlined into the RUN.container freeform string as is the case with the nginx volume in the examples?
			sKey := strings.Split(val.Key, ".")
			containerPort := sKey[len(sKey)-1]
			ports = append(ports, fmt.Sprintf("%s:%s", value, containerPort))

			continue
		}

		if strings.HasPrefix(val.Key, "RUN.volume.") {
			if v := strings.TrimSpace(value); v != "" {
				extraVolumes = append(extraVolumes, v)
			}

			continue
		}

		if strings.HasPrefix(val.Key, "RUN.env.") {
			sKey := strings.Split(val.Key, ".")
			envKey := sKey[len(sKey)-1]
			env = append(env, fmt.Sprintf("%s=%s", envKey, value))

			continue
		}

		if strings.HasPrefix(val.Key, "guestinfo.") {
			key := strings.Replace(strings.ToUpper(val.Key), ".", "_", -1)
			env = append(env, fmt.Sprintf("VMX_%s=%s", key, value))

			continue
		}
	}

	if len(args) == 0 {
		// not an error - it's simply a simVM that shouldn't be backed by a container
		return nil
	}

	// cloud-init's VMware datasource reads guestinfo from the environment only
	// when VMX_GUESTINFO=true.
	env = append(env, "VMX_GUESTINFO=true")

	volumes := []string{}
	if mountDMI {
		volumes = append(volumes, constructVolumeName(ctx.Map.containerName(svm.vm.Name, svm.vm.uid.String()), "dmi")+":/sys/class/dmi/id")
	}
	volumes = append(volumes, extraVolumes...)

	if mountMachineID {
		mounts, err := svm.carryBIOSUUID(selectBIOSUUIDCarrier())
		if err != nil {
			return err
		}
		volumes = append(volumes, mounts...)
	} else {
		log.Printf("%s: BIOS UUID carrier none: RUN.machineId is false", svm.vm.Name)
	}

	if vmci {
		dir, err := os.MkdirTemp("", "vcsim-rpc-")
		if err != nil {
			svm.removeMachineID()
			return fmt.Errorf("guestrpc: %w", err)
		}
		svm.guestRPCDir = dir

		// The directory is mounted, not the socket, because a file mount
		// target cannot be created inside the fresh tmpfs that
		// RUN.nestedContainers puts at /run.
		volumes = append(volumes, dir+":"+path.Dir(toolbox.UnixSocketPath))
		env = append(env, toolbox.UnixSocketEnv+"="+toolbox.UnixSocketPath)

		if !hasVolumeDest(svm.vm.Config.ExtraConfig, rpctoolPath) {
			if bin, err := buildToolbox(); err != nil {
				log.Printf("%s: %s not mounted: %s", svm.vm.Name, rpctoolPath, err)
			} else {
				volumes = append(volumes, bin+":"+rpctoolPath+":ro")
			}
		}
	}

	nics, err := svm.containerNICs(ctx)
	if err != nil {
		svm.removeGuestRPC()
		svm.removeMachineID()
		return err
	}

	svm.c, err = create(ctx, svm.vm.Name, svm.vm.uid.String(), args[0], args[1:], createOptions{
		Networks:          networks,
		NICs:              nics,
		CapAdd:            capAdd(ctx),
		Volumes:           volumes,
		Ports:             ports,
		Env:               env,
		Privileged:        privileged,
		NestedContainers:  nestedContainers,
		QuoteImageAndArgs: jsonArgs,
		RecreateIfExists:  true,
	})
	if err != nil {
		svm.removeGuestRPC()
		svm.removeMachineID()
		return err
	}

	if mountDMI {
		// not combined with the test assembling volumes because we want to have the container name first.
		// cannot add a label to a volume after creation, so if we want to associate with the container ID the
		// container must come first
		err = svm.populateDMI()
		if err != nil {
			return err
		}
	}

	if err = svm.startGuestRPC(ctx); err != nil {
		return err
	}

	err = svm.c.start(ctx)
	if err != nil {
		log.Printf("%s %s: %s %s", svm.vm.Name, "start", args, err)
		svm.stopGuestRPC()
		return err
	}

	svm.updateToolsStatus(ctx, false)

	svm.vm.logPrintf("%s: %s", args, svm.c.id)

	// Sync network config, retrying a few times to allow the container to get an IP
	// Container runtimes may take a moment to assign an IP address after start
	for range 5 {
		if err = svm.syncNetworkConfigToVMGuestProperties(ctx, nil, nil); err != nil {
			log.Printf("%s inspect %s: %s", svm.vm.Name, svm.c.id, err)
			break
		}
		if svm.vm.Guest.IpAddress != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	callback := func(callbackCtx *Context, out []byte, details *containerDetails, c *container) error {
		// c.id is written under c.Lock() in container.remove().
		c.Lock()
		containerGone := c.id == ""
		c.Unlock()

		if containerGone && svm.vm != nil {
			// If the container cannot be found then destroy this VM unless the VM is no longer configured for container backing (svm.vm == nil)
			taskRef := svm.vm.DestroyTask(callbackCtx, &types.Destroy_Task{This: svm.vm.Self}).(*methods.Destroy_TaskBody).Res.Returnval
			task, ok := callbackCtx.Map.Get(taskRef).(*Task)
			if !ok {
				panic(fmt.Sprintf("couldn't retrieve task for moref %+q while deleting VM %s", taskRef, svm.vm.Name))
			}

			// Wait for the task to complete and see if there is an error.
			task.Wait()
			if task.Info.Error != nil {
				err := fmt.Errorf("failed to destroy vm: %v", task.Info.Error)
				svm.vm.logPrintf("%s", err.Error())
				return err
			}
		}

		if containerGone {
			return svm.syncNetworkConfigToVMGuestProperties(callbackCtx, out, details)
		}

		// The watcher inspected the container without the VM's lock, so a
		// power operation may have changed the container since, and applying
		// that result would undo the operation's power state. Inspect again
		// under the lock, which power operations hold.
		var err error
		callbackCtx.WithLock(svm.vm, func() {
			err = svm.syncNetworkConfigToVMGuestProperties(callbackCtx, nil, nil)
		})
		return err
	}

	// Start watching the container resource.
	err = svm.c.watchContainer(ctx, callback)
	if isUninitializedContainer(err) {
		// the container has been deleted before we could watch, despite successful launch so clean up.
		callback(ctx, nil, nil, svm.c)

		// successful launch so nil the error
		return nil
	}

	return err
}

// stop the container (if any) for the given vm.
func (svm *simVM) stop(ctx *Context) error {
	if svm == nil || svm.c == nil {
		return nil
	}

	defer svm.stopGuestRPC()

	err := svm.c.stop(ctx)
	if err != nil {
		log.Printf("%s %s: %s", svm.vm.Name, "stop", err)

		return err
	}

	ctx.Update(svm.vm, toolsNotRunning)

	return nil
}

// pause the container (if any) for the given vm.
func (svm *simVM) pause(ctx *Context) error {
	if svm == nil || svm.c == nil {
		return nil
	}

	err := svm.c.pause(ctx)
	if err != nil {
		log.Printf("%s %s: %s", svm.vm.Name, "pause", err)

		return err
	}

	svm.updateToolsStatus(ctx, true)

	return nil
}

// restart the container (if any) for the given vm.
func (svm *simVM) restart(ctx *Context) error {
	if svm == nil || svm.c == nil {
		return nil
	}

	err := svm.c.restart(ctx)
	if err != nil {
		log.Printf("%s %s: %s", svm.vm.Name, "restart", err)

		return err
	}

	if svm.guestRPC != nil {
		// The guest's tools restart with it, and report again once they
		// have; until then they are not running.
		ctx.Update(svm.vm, toolsNotRunning)
	} else {
		ctx.Update(svm.vm, toolsRunning)
	}

	return nil
}

// remove the container (if any) for the given vm.
func (svm *simVM) remove(ctx *Context) error {
	if svm == nil || svm.c == nil {
		return nil
	}

	defer svm.removeGuestRPC()
	defer svm.removeMachineID()

	err := svm.c.remove(ctx)
	if err != nil {
		log.Printf("%s %s: %s", svm.vm.Name, "remove", err)

		return err
	}

	return nil
}

// startGuestRPC serves GuestRPC in svm.guestRPCDir, if the VM has one and it
// is not already served.
func (svm *simVM) startGuestRPC(ctx *Context) error {
	if svm.guestRPCDir == "" || svm.guestRPC != nil {
		return nil
	}

	srv := newGuestRPCServer(svm.vm, filepath.Join(svm.guestRPCDir, path.Base(toolbox.UnixSocketPath)))
	svm.toolsNicInfo.Store(false)
	srv.reported = &svm.toolsNicInfo
	if err := srv.start(ctx); err != nil {
		return err
	}
	svm.guestRPC = srv

	return nil
}

// updateToolsStatus reports the tools status after the container has started
// or resumed (paused false) or has been paused (paused true). With a GuestRPC
// channel the status is what the guest's tools have reported, which survives
// a pause; without one it follows the container.
func (svm *simVM) updateToolsStatus(ctx *Context, paused bool) {
	if svm.guestRPC == nil {
		ctx.Update(svm.vm, toolsStatusChanges(!paused))
		return
	}
	svm.guestRPC.setPaused(paused)
	ctx.Update(svm.vm, toolsStatusChanges(svm.guestRPC.tools.running()))
}

// stopGuestRPC stops serving GuestRPC, keeping svm.guestRPCDir for a restart.
func (svm *simVM) stopGuestRPC() {
	if svm.guestRPC != nil {
		svm.guestRPC.stop()
		svm.guestRPC = nil
	}
}

// removeGuestRPC stops serving GuestRPC and removes svm.guestRPCDir.
func (svm *simVM) removeGuestRPC() {
	svm.stopGuestRPC()

	if svm.guestRPCDir != "" {
		_ = os.RemoveAll(svm.guestRPCDir)
		svm.guestRPCDir = ""
	}
}

// biosUUIDCarrier is how a container VM's guest is given its BIOS UUID, for
// RUN.machineId, where it has no DMI data from RUN.mountdmi.
type biosUUIDCarrier string

// carrierMachineID mounts the BIOS UUID as the guest's machine ID; see
// createMachineID.
const carrierMachineID biosUUIDCarrier = "machine-id"

// selectBIOSUUIDCarrier picks the BIOS UUID carrier for this host. Only
// machine-id is implemented. A DMI carrier, bind-mounting product_uuid and
// the related files at /sys/class/dmi/id, is to be chosen here on hosts that
// have /sys/class/dmi/id (spec 95 D-95-32); WSL2 has none.
func selectBIOSUUIDCarrier() biosUUIDCarrier {
	return carrierMachineID
}

// carryBIOSUUID prepares carrier for the VM and returns the mounts that give
// the guest its BIOS UUID. It logs the carrier, so that one is never chosen
// silently.
func (svm *simVM) carryBIOSUUID(carrier biosUUIDCarrier) ([]string, error) {
	var mounts []string
	var err error

	switch carrier {
	case carrierMachineID:
		mounts, err = svm.createMachineID()
	default:
		return nil, fmt.Errorf("unknown BIOS UUID carrier %q", carrier)
	}
	if err != nil {
		return nil, err
	}

	if len(mounts) == 0 {
		log.Printf("%s: BIOS UUID carrier %s: none mounted, RUN.volume targets every path", svm.vm.Name, carrier)
	} else {
		log.Printf("%s: BIOS UUID carrier %s: %s", svm.vm.Name, carrier, strings.Join(mounts, " "))
	}

	return mounts, nil
}

// machineIDPaths are where vcsim mounts the machine ID for RUN.machineId:
// systemd's and D-Bus's (machine-id(5)).
var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// createMachineID creates svm.machineIDDir holding the VM's machine ID and
// returns the read-only mounts of it at each of machineIDPaths that no
// RUN.volume targets. It returns no mounts, and creates nothing, if every
// path is taken.
//
// A host file is bind-mounted, as for GuestRPC, because a volume cannot be
// mounted at a file, and the file must be in place before the container
// starts, since systemd and D-Bus read the machine ID at boot.
func (svm *simVM) createMachineID() ([]string, error) {
	var dests []string
	for _, p := range machineIDPaths {
		if !hasVolumeDest(svm.vm.Config.ExtraConfig, p) {
			dests = append(dests, p)
		}
	}
	if len(dests) == 0 {
		return nil, nil
	}

	dir, err := os.MkdirTemp("", "vcsim-machine-id-")
	if err != nil {
		return nil, fmt.Errorf("machine-id: %w", err)
	}
	// MkdirTemp creates the directory 0700, which a container user that is
	// not the host user, as under rootless podman, could not traverse.
	if err = os.Chmod(dir, 0755); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("machine-id: %w", err)
	}
	svm.machineIDDir = dir

	if err = svm.writeMachineID(); err != nil {
		svm.removeMachineID()
		return nil, err
	}

	file := filepath.Join(dir, "machine-id")
	mounts := make([]string, len(dests))
	for i, p := range dests {
		mounts[i] = file + ":" + p + ":ro"
	}

	return mounts, nil
}

// writeMachineID writes the VM's machine ID, its BIOS UUID as machine-id(5)
// formats it, to the file in svm.machineIDDir, if there is one. The file is
// rewritten in place, so a mount of it sees the change.
func (svm *simVM) writeMachineID() error {
	if svm.machineIDDir == "" {
		return nil
	}

	id := svm.biosUUID()
	data := hex.EncodeToString(id[:]) + "\n"
	if err := os.WriteFile(filepath.Join(svm.machineIDDir, "machine-id"), []byte(data), 0644); err != nil {
		return fmt.Errorf("machine-id: %w", err)
	}

	return nil
}

// removeMachineID removes svm.machineIDDir.
func (svm *simVM) removeMachineID() {
	if svm.machineIDDir != "" {
		_ = os.RemoveAll(svm.machineIDDir)
		svm.machineIDDir = ""
	}
}

// rpctoolPath is where vcsim mounts the toolbox binary for RUN.vmci.
const rpctoolPath = "/usr/bin/vmware-rpctool"

// hasVolumeDest reports whether a RUN.volume.* key in extraConfig mounts
// something at dest.
func hasVolumeDest(extraConfig []types.BaseOptionValue, dest string) bool {
	for _, opt := range extraConfig {
		v := opt.GetOptionValue()
		if !strings.HasPrefix(v.Key, "RUN.volume.") {
			continue
		}
		if parts := strings.SplitN(fmt.Sprint(v.Value), ":", 3); len(parts) >= 2 && parts[1] == dest {
			return true
		}
	}
	return false
}

// parseBoolOption sets *dst from a JSON boolean, leaving it unchanged when
// value is not one.
func parseBoolOption(value string, dst *bool) {
	var b bool
	if json.Unmarshal([]byte(value), &b) == nil {
		*dst = b
	}
}

func (svm *simVM) exec(ctx *Context, auth types.BaseGuestAuthentication, args []string) (string, types.BaseMethodFault) {
	if svm == nil || svm.c == nil {
		return "", nil
	}

	fault := svm.prepareGuestOperation(auth)
	if fault != nil {
		return "", fault
	}

	out, err := svm.c.exec(ctx, args)
	if err != nil {
		log.Printf("%s: %s (%s)", svm.vm.Name, args, string(out))
		return "", new(types.GuestOperationsFault)
	}

	return strings.TrimSpace(string(out)), nil
}

func guestUpload(id string, file string, r *http.Request) error {
	// TODO: decide behaviour for no container
	err := copyToGuest(id, file, r.ContentLength, r.Body)
	_ = r.Body.Close()
	return err
}

func guestDownload(id string, file string, w http.ResponseWriter) error {
	// TODO: decide behaviour for no container
	sink := func(len int64, r io.Reader) error {
		w.Header().Set("Content-Length", strconv.FormatInt(len, 10))
		_, err := io.Copy(w, r)
		return err
	}

	err := copyFromGuest(id, file, sink)
	return err
}

const guestPrefix = "/guestFile/"

// ServeGuest handles container guest file upload/download
func ServeGuest(w http.ResponseWriter, r *http.Request) {
	// Real vCenter form: /guestFile?id=139&token=...
	// vcsim form:        /guestFile/tmp/foo/bar?id=ebc8837b8cb6&token=...

	id := r.URL.Query().Get("id")
	file := strings.TrimPrefix(r.URL.Path, guestPrefix[:len(guestPrefix)-1])
	var err error

	switch r.Method {
	case http.MethodPut:
		err = guestUpload(id, file, r)
	case http.MethodGet:
		err = guestDownload(id, file, w)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if err != nil {
		log.Printf("%s %s: %s", r.Method, r.URL, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// productSerial returns the uuid in /sys/class/dmi/id/product_serial format
func productSerial(id uuid.UUID) string {
	var dst [len(id)*2 + len(id) - 1]byte

	j := 0
	for i := range len(id) {
		hex.Encode(dst[j:j+2], id[i:i+1])
		j += 3
		if j < len(dst) {
			s := j - 1
			if s == len(dst)/2 {
				dst[s] = '-'
			} else {
				dst[s] = ' '
			}
		}
	}

	return fmt.Sprintf("VMware-%s", string(dst[:]))
}

// productUUID returns the uuid in /sys/class/dmi/id/product_uuid format
func productUUID(id uuid.UUID) string {
	var dst [36]byte

	hex.Encode(dst[0:2], id[3:4])
	hex.Encode(dst[2:4], id[2:3])
	hex.Encode(dst[4:6], id[1:2])
	hex.Encode(dst[6:8], id[0:1])
	dst[8] = '-'
	hex.Encode(dst[9:11], id[5:6])
	hex.Encode(dst[11:13], id[4:5])
	dst[13] = '-'
	hex.Encode(dst[14:16], id[7:8])
	hex.Encode(dst[16:18], id[6:7])
	dst[18] = '-'
	hex.Encode(dst[19:23], id[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:], id[10:])

	return strings.ToUpper(string(dst[:]))
}

// firstNICMacAddress returns the MAC address of vm's first virtual Ethernet
// card, or "" if it has none.
func firstNICMacAddress(vm *VirtualMachine) string {
	for _, d := range vm.Config.Hardware.Device {
		if eth, ok := d.(types.BaseVirtualEthernetCard); ok {
			return eth.GetVirtualEthernetCard().MacAddress
		}
	}
	return ""
}
