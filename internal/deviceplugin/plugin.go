// Package deviceplugin implements a Kubernetes device plugin that advertises
// the KVM device (agentrun.dev/kvm) to the kubelet and injects /dev/kvm (and
// /dev/net/tun) into containers that request it.
//
// The point of the plugin is to give husk pods access to /dev/kvm WITHOUT
// running privileged: true. A pod requests the resource (agentrun.dev/kvm: 1)
// like any other extended resource; the scheduler only places it on a node
// whose plugin advertised healthy capacity (scheduler truth: a node with no
// /dev/kvm advertises zero and never gets the pod), and the plugin injects the
// device node on Allocate. This is the PSA-restricted path: the only deviation
// from the restricted profile is the documented device exception, not a blanket
// privileged container.
package deviceplugin

import (
	"context"
	"fmt"

	v1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Plugin implements the v1beta1 DevicePlugin gRPC service for the KVM device.
//
// It advertises deviceCount synthetic device slots (ids kvm-0..kvm-{N-1}) as
// long as kvmPresent() reports the host /dev/kvm exists, and zero otherwise.
// On Allocate it returns a DeviceSpec per configured device path so the kubelet
// bind-mounts those host device nodes into the container.
type Plugin struct {
	v1beta1.UnimplementedDevicePluginServer

	// resourceName is the extended resource the plugin advertises, e.g.
	// agentrun.dev/kvm. Pods request it under spec.containers[].resources.
	resourceName string
	// deviceCount is how many synthetic slots to advertise when KVM is present.
	// /dev/kvm is shareable, so the count is a soft concurrency cap, not a count
	// of physical devices; a value like 100 lets many husk pods share one node.
	deviceCount int
	// devicePaths are the host device nodes injected into a container on
	// Allocate, e.g. /dev/kvm and /dev/net/tun. Each is mounted at the same
	// container path with rw permissions.
	devicePaths []string
	// kvmPresent reports whether the host /dev/kvm exists. Injectable so tests
	// can drive the present/absent branches without a real device node.
	kvmPresent func() bool
}

// NewPlugin builds a Plugin. kvmPresent must be non-nil (the caller decides how
// to probe the host, e.g. stat /dev/kvm).
func NewPlugin(resourceName string, deviceCount int, devicePaths []string, kvmPresent func() bool) *Plugin {
	return &Plugin{
		resourceName: resourceName,
		deviceCount:  deviceCount,
		devicePaths:  devicePaths,
		kvmPresent:   kvmPresent,
	}
}

// ResourceName returns the extended resource the plugin advertises.
func (p *Plugin) ResourceName() string { return p.resourceName }

// GetDevicePluginOptions returns the plugin options. The KVM plugin needs no
// pre-start hook and offers no preferred-allocation logic, so both flags stay
// false (the kubelet then never calls PreStartContainer or
// GetPreferredAllocation).
func (p *Plugin) GetDevicePluginOptions(context.Context, *v1beta1.Empty) (*v1beta1.DevicePluginOptions, error) {
	return &v1beta1.DevicePluginOptions{
		PreStartRequired:                false,
		GetPreferredAllocationAvailable: false,
	}, nil
}

// devices builds the device list the plugin advertises right now: deviceCount
// healthy slots when /dev/kvm is present, an empty slice otherwise.
func (p *Plugin) devices() []*v1beta1.Device {
	if !p.kvmPresent() {
		// No /dev/kvm on this node: advertise zero so the scheduler never
		// places a pod requesting the resource here. Honest scheduler truth.
		return []*v1beta1.Device{}
	}
	devs := make([]*v1beta1.Device, 0, p.deviceCount)
	for i := 0; i < p.deviceCount; i++ {
		devs = append(devs, &v1beta1.Device{
			ID:     fmt.Sprintf("kvm-%d", i),
			Health: v1beta1.Healthy,
		})
	}
	return devs
}

// ListAndWatch streams the device list to the kubelet. It sends the current
// list once, then blocks until the stream context is cancelled (the kubelet
// closes the stream on shutdown or re-registration).
//
// This is the simple, documented version: it does not watch /dev/kvm for live
// appearance/disappearance and re-send on a health change. A node that gains or
// loses /dev/kvm is reflected the next time the plugin (re)registers and the
// kubelet reopens the stream, which is sufficient because /dev/kvm presence is
// a boot-time property of a node in practice. A future revision can add a
// health watcher that re-sends on change.
func (p *Plugin) ListAndWatch(_ *v1beta1.Empty, srv v1beta1.DevicePlugin_ListAndWatchServer) error {
	if err := srv.Send(&v1beta1.ListAndWatchResponse{Devices: p.devices()}); err != nil {
		return fmt.Errorf("device plugin: send ListAndWatch response: %w", err)
	}
	<-srv.Context().Done()
	return nil
}

// Allocate is called when the kubelet admits a pod that requested the resource.
// For each requested container it returns a ContainerAllocateResponse with a
// DeviceSpec per configured device path so the kubelet bind-mounts /dev/kvm
// (and /dev/net/tun) into the container at the same path with rw permissions.
//
// The requested device IDs are the synthetic slots from ListAndWatch; they
// select how many concurrent claims the node admits, but every claim maps to
// the same underlying host device nodes, so the response does not vary by id.
func (p *Plugin) Allocate(_ context.Context, req *v1beta1.AllocateRequest) (*v1beta1.AllocateResponse, error) {
	resp := &v1beta1.AllocateResponse{
		ContainerResponses: make([]*v1beta1.ContainerAllocateResponse, 0, len(req.GetContainerRequests())),
	}
	for range req.GetContainerRequests() {
		specs := make([]*v1beta1.DeviceSpec, 0, len(p.devicePaths))
		for _, path := range p.devicePaths {
			specs = append(specs, &v1beta1.DeviceSpec{
				HostPath:      path,
				ContainerPath: path,
				Permissions:   "rw",
			})
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &v1beta1.ContainerAllocateResponse{
			Devices: specs,
		})
	}
	return resp, nil
}

// PreStartContainer is a no-op: GetDevicePluginOptions reports
// PreStartRequired=false, so the kubelet never calls this, but the method is
// implemented to satisfy the service contract.
func (p *Plugin) PreStartContainer(context.Context, *v1beta1.PreStartContainerRequest) (*v1beta1.PreStartContainerResponse, error) {
	return &v1beta1.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation is a no-op: GetDevicePluginOptions reports
// GetPreferredAllocationAvailable=false, so the kubelet never calls this. It
// returns an empty response to satisfy the service contract.
func (p *Plugin) GetPreferredAllocation(context.Context, *v1beta1.PreferredAllocationRequest) (*v1beta1.PreferredAllocationResponse, error) {
	return &v1beta1.PreferredAllocationResponse{}, nil
}
