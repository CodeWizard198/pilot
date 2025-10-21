package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	DefaultTimeout = 5 * time.Second
)

type Watcher struct {
	client    *clientv3.Client
	eventChan chan *ServiceEvent
	ctx       context.Context
	cancel    context.CancelFunc

	serviceMetadataPrefix  string
	serviceDiscoveryPrefix string

	metadataMap     map[string]*ServiceMetadata
	instancesMap    map[string][]*ServiceInstance
	initialLoading  bool

	mu sync.RWMutex
}

// NewWatcher 创建一个新的 etcd 监听器
func NewWatcher(endpoints []string, dialTimeout time.Duration, serviceMetadataPrefix, serviceDiscoveryPrefix string) (*Watcher, error) {
	if dialTimeout <= 0 {
		dialTimeout = DefaultTimeout
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := &Watcher{
		client:                 client,
		eventChan:              make(chan *ServiceEvent, 100),
		ctx:                    ctx,
		cancel:                 cancel,
		serviceMetadataPrefix:  serviceMetadataPrefix,
		serviceDiscoveryPrefix: serviceDiscoveryPrefix,
		metadataMap:            make(map[string]*ServiceMetadata),
		instancesMap:           make(map[string][]*ServiceInstance),
	}

	return w, nil
}

// Start 开始监听 etcd 中的服务变更
func (w *Watcher) Start() error {
	// First, load existing services and instances
	if err := w.loadExistingServicesAndInstances(); err != nil {
		return fmt.Errorf("failed to load existing services: %w", err)
	}

	// Start watching both paths
	go w.watchMetadata()
	go w.watchInstances()

	return nil
}

// loadExistingServicesAndInstances 从 etcd 加载所有现有的服务和实例
func (w *Watcher) loadExistingServicesAndInstances() error {
	ctx, cancel := context.WithTimeout(w.ctx, DefaultTimeout)
	defer cancel()

	// mark initial loading
	w.mu.Lock()
	w.initialLoading = true
	w.mu.Unlock()

	// Load service metadata from w.serviceMetadataPrefix
	resp, err := w.client.Get(ctx, w.serviceMetadataPrefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get service metadata: %w", err)
	}

	for _, kv := range resp.Kvs {
		w.handleMetadataEvent(&clientv3.Event{
			Type: clientv3.EventTypePut,
			Kv:   kv,
		})
	}

	// 在 w.serviceDiscoveryPrefix 中加载所有实例
	resp, err = w.client.Get(ctx, w.serviceDiscoveryPrefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get service instances: %w", err)
	}

	for _, kv := range resp.Kvs {
		w.handleInstanceEvent(&clientv3.Event{
			Type: clientv3.EventTypePut,
			Kv:   kv,
		})
	}

	// 结束初次加载，开始发出新增事件
	w.mu.Lock()
	w.initialLoading = false
	w.mu.Unlock()

	// 发送所有服务的初始事件 包括元数据和实例
	w.mu.RLock()
	defer w.mu.RUnlock()

	for serviceName, metadata := range w.metadataMap {
		instances := w.instancesMap[serviceName]
		if instances == nil {
			instances = make([]*ServiceInstance, 0)
		}

		serviceInfo := &ServiceInfo{
			ServiceMetadata: metadata,
			Instances:       instances,
		}

		w.eventChan <- &ServiceEvent{
			Type:    EventAdd,
			Service: serviceInfo,
		}
	}

	return nil
}

// watchMetadata 监听 w.serviceMetadataPrefix 路径下的服务元数据变更
func (w *Watcher) watchMetadata() {
	watchChan := w.client.Watch(w.ctx, w.serviceMetadataPrefix, clientv3.WithPrefix())

	for {
		select {
		case <-w.ctx.Done():
			return
		case watchResp := <-watchChan:
			if watchResp.Err() != nil {
				log.Printf("Metadata watch error: %v", watchResp.Err())
				continue
			}

			for _, event := range watchResp.Events {
				w.handleMetadataEvent(event)
			}
		}
	}
}

// watchInstances 监听 w.serviceDiscoveryPrefix 路径下的服务实例变更
func (w *Watcher) watchInstances() {
	watchChan := w.client.Watch(w.ctx, w.serviceDiscoveryPrefix, clientv3.WithPrefix())

	for {
		select {
		case <-w.ctx.Done():
			return
		case watchResp := <-watchChan:
			if watchResp.Err() != nil {
				log.Printf("Instance watch error: %v", watchResp.Err())
				continue
			}

			for _, event := range watchResp.Events {
				w.handleInstanceEvent(event)
			}
		}
	}
}

// handleMetadataEvent 处理来自 /w.serviceMetadataPrefix 的服务元数据事件
func (w *Watcher) handleMetadataEvent(event *clientv3.Event) {
	key := string(event.Kv.Key)
	serviceName := w.extractServiceNameFromMetadata(key)
	if serviceName == "" {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	switch event.Type {
	case clientv3.EventTypePut:
		metadata, err := w.parseServiceMetadata(event.Kv.Value)
		if err != nil {
			log.Printf("Failed to parse service metadata for %s: %v", serviceName, err)
			return
		}

		w.metadataMap[serviceName] = metadata

		instances := w.instancesMap[serviceName]
		if instances == nil {
			instances = make([]*ServiceInstance, 0)
		}

		serviceInfo := &ServiceInfo{
			ServiceMetadata: metadata,
			Instances:       instances,
		}

		// during initial loading, do not emit updates
		if w.initialLoading {
			return
		}
		w.eventChan <- &ServiceEvent{
			Type:    EventUpdate,
			Service: serviceInfo,
		}

	case clientv3.EventTypeDelete:
		metadata, exists := w.metadataMap[serviceName]
		if !exists {
			return
		}

		delete(w.metadataMap, serviceName)

		// 如果没有实例 发送删除事件
		instances := w.instancesMap[serviceName]
		if len(instances) == 0 {
			serviceInfo := &ServiceInfo{
				ServiceMetadata: metadata,
				Instances:       instances,
			}
			w.eventChan <- &ServiceEvent{
				Type:    EventDelete,
				Service: serviceInfo,
			}
		}
	}
}

// handleInstanceEvent 处理来自 /w.serviceDiscoveryPrefix 的服务实例事件
func (w *Watcher) handleInstanceEvent(event *clientv3.Event) {
	key := string(event.Kv.Key)
	serviceName := w.extractServiceNameFromInstance(key)
	if serviceName == "" {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	switch event.Type {
	case clientv3.EventTypePut:
		value := string(event.Kv.Value)
		instance := w.parseServiceInstance(value)
		if instance == nil {
			return
		}

		instances := w.instancesMap[serviceName]
		if instances == nil {
			instances = make([]*ServiceInstance, 0)
		}

		found := false
		for i, inst := range instances {
			if inst.Addr == instance.Addr {
				instances[i] = instance
				found = true
				break
			}
		}

		if !found {
			instances = append(instances, instance)
		}

		w.instancesMap[serviceName] = instances

		metadata := w.metadataMap[serviceName]
		if metadata == nil {
			return
		}

		serviceInfo := &ServiceInfo{
			ServiceMetadata: metadata,
			Instances:       instances,
		}

		if w.initialLoading {
			return
		}
		w.eventChan <- &ServiceEvent{
			Type:    EventUpdate,
			Service: serviceInfo,
		}

	case clientv3.EventTypeDelete:
		instances := w.instancesMap[serviceName]
		if instances == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		resp, err := w.client.Get(ctx, w.serviceDiscoveryPrefix+serviceName+"/", clientv3.WithPrefix())
		cancel()

		if err != nil {
			log.Printf("Failed to fetch instances for service %s: %v", serviceName, err)
			return
		}

		newInstances := make([]*ServiceInstance, 0)
		for _, kv := range resp.Kvs {
			value := string(kv.Value)
			instance := w.parseServiceInstance(value)
			if instance != nil {
				newInstances = append(newInstances, instance)
			}
		}

		w.instancesMap[serviceName] = newInstances

		metadata := w.metadataMap[serviceName]
		if metadata == nil {
			log.Printf("No metadata found for service %s", serviceName)
			return
		}

		if len(newInstances) == 0 {
			serviceInfo := &ServiceInfo{
				ServiceMetadata: metadata,
				Instances:       newInstances,
			}

			w.eventChan <- &ServiceEvent{
				Type:    EventDelete,
				Service: serviceInfo,
			}
		} else {
			serviceInfo := &ServiceInfo{
				ServiceMetadata: metadata,
				Instances:       newInstances,
			}

			// during initial loading, do not emit updates
			if w.initialLoading {
				return
			}
			w.eventChan <- &ServiceEvent{
				Type:    EventUpdate,
				Service: serviceInfo,
			}
		}
	}
}

// parseServiceMetadata 解析服务元数据，从 /w.serviceMetadataPrefix/{service_name} 中解析服务元数据
func (w *Watcher) parseServiceMetadata(data []byte) (*ServiceMetadata, error) {
	var metadata ServiceMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal service metadata: %w", err)
	}

	// Parse the protobuf descriptor
	if len(metadata.DescriptorData) > 0 {
		var fds descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(metadata.DescriptorData, &fds); err != nil {
			return nil, fmt.Errorf("failed to unmarshal descriptor: %w", err)
		}
		metadata.Descriptor = &fds
	}

	return &metadata, nil
}

// parseServiceInstance 从 /w.serviceDiscoveryPrefix/{service_name}/{instance_id} 中解析服务实例
func (w *Watcher) parseServiceInstance(value string) *ServiceInstance {
	// Format: "host:port"
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		log.Printf("Invalid service instance value format: %s", value)
		return nil
	}

	address := parts[0]
	var port int
	if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil {
		log.Printf("Invalid port in service instance: %s", value)
		return nil
	}

	return &ServiceInstance{
		Address: address,
		Port:    port,
		Addr:    value,
	}
}

// extractServiceNameFromMetadata 从 /w.serviceMetadataPrefix/{service_name} 中提取服务名
func (w *Watcher) extractServiceNameFromMetadata(key string) string {
	if !strings.HasPrefix(key, w.serviceMetadataPrefix) {
		return ""
	}

	serviceName := key[len(w.serviceMetadataPrefix):]
	// Remove trailing slash if any
	serviceName = strings.TrimSuffix(serviceName, "/")
	return serviceName
}

// extractServiceNameFromInstance 从 /w.serviceDiscoveryPrefix/{service_name}/{instance_id} 中提取服务名
func (w *Watcher) extractServiceNameFromInstance(key string) string {
	if !strings.HasPrefix(key, w.serviceDiscoveryPrefix) {
		return ""
	}

	remaining := key[len(w.serviceDiscoveryPrefix):]
	parts := strings.Split(remaining, "/")

	if len(parts) >= 1 {
		return parts[0]
	}

	return ""
}

// Events 返回用于接收服务事件的通道
func (w *Watcher) Events() <-chan *ServiceEvent {
	return w.eventChan
}

// GetService 根据名称获取服务，包括元数据和实例
func (w *Watcher) GetService(serviceName string) *ServiceInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()

	metadata, metadataExists := w.metadataMap[serviceName]
	instances, instancesExist := w.instancesMap[serviceName]

	if !metadataExists && !instancesExist {
		return nil
	}

	if !metadataExists {
		metadata = &ServiceMetadata{
			ServiceName: serviceName,
		}
	}

	if !instancesExist {
		instances = make([]*ServiceInstance, 0)
	}

	return &ServiceInfo{
		ServiceMetadata: metadata,
		Instances:       instances,
	}
}

// Stop 停止监听器
func (w *Watcher) Stop() error {
	w.cancel()
	close(w.eventChan)
	return w.client.Close()
}
