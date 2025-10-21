package router

import (
	"fmt"
	"pilot/internal/discovery"
	"pilot/internal/transcoder"
	"sync"
	"sync/atomic"
)

// ServicePool 服务池 用于负载均衡调用
// 维护一个服务名下的所有实例与其 invoker。
type ServicePool struct {
	serviceName string
	invokers    map[string]*transcoder.GRPCInvoker // key: ip地址
	instances   []*discovery.ServiceInstance
	counter     int64
	mu          sync.RWMutex
}

// getNextInvoker 获取下一个可用的 invoker
func (pool *ServicePool) getNextInvoker() (*transcoder.GRPCInvoker, error) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	if len(pool.instances) == 0 {
		return nil, fmt.Errorf("no available instances")
	}

	start := atomic.AddInt64(&pool.counter, 1)
	n := len(pool.instances)
	for i := range n {
		idx := int((start + int64(i)) % int64(n))
		instance := pool.instances[idx]
		if invoker, ok := pool.invokers[instance.Addr]; ok && invoker != nil {
			return invoker, nil
		}
	}
	return nil, fmt.Errorf("no invoker available for service %s", pool.serviceName)
}
