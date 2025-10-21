package discovery

import "google.golang.org/protobuf/types/descriptorpb"

type ServiceInstance struct {
	Address string
	Port    int
	Addr    string // "host:port"
}

type ServiceMetadata struct {
	ServiceName    string                          `json:"service_name"`
	Descriptor     *descriptorpb.FileDescriptorSet `json:"-"`
	DescriptorData []byte                          `json:"descriptor_data"` // base64 encoded protobuf descriptor
	Version        string                          `json:"version"`
	Metadata       map[string]string               `json:"metadata"`
}

// ServiceInfo combines metadata and instances
type ServiceInfo struct {
	*ServiceMetadata
	Instances []*ServiceInstance
}

// ServiceEvent represents a service registration/deregistration event
type ServiceEvent struct {
	Type    EventType
	Service *ServiceInfo
}

// EventType defines the type of service event
type EventType int

const (
	EventAdd EventType = iota
	EventDelete
	EventUpdate
)
