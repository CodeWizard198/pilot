package router

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"

	"pilot/internal/discovery"
	"pilot/internal/transcoder"

	"github.com/jhump/protoreflect/desc"
)

// Route 路由节点
type Route struct {
	ServiceName string
	MethodName  string
	FullMethod  string
	MethodDesc  *desc.MethodDescriptor
	HttpRule    *transcoder.HTTPRule
}

// HTTPRouter 路由树及索引
type HTTPRouter struct {
	routerTree   *RouteTree[*Route]
	servicePools map[string]*ServicePool
	routeIndex   map[string]map[string]struct{} // serviceName -> set(path)
	pathIndex    map[string]struct{}            // global path set for fast existence check
	mu           sync.RWMutex
}

func NewHTTPRouter() *HTTPRouter {
	return &HTTPRouter{
		routerTree:        NewRouteTree[*Route](),
		servicePools:      make(map[string]*ServicePool),
		routeIndex:        make(map[string]map[string]struct{}),
		pathIndex:         make(map[string]struct{}),
	}
}

// RegisterService 注册服务与其路由。
// 首次注册该 serviceName 时,根据描述符解析并注册路由,后续仅更新实例池
func (r *HTTPRouter) RegisterService(service *discovery.ServiceInfo) error {
	if service == nil || strings.TrimSpace(service.ServiceName) == "" {
		return fmt.Errorf("invalid service info")
	}

	serviceName := service.ServiceName

	// 确保 ServicePool 存在
	r.mu.Lock()
	pool, exists := r.servicePools[serviceName]
	if !exists {
		pool = &ServicePool{
			serviceName: serviceName,
			invokers:    make(map[string]*transcoder.GRPCInvoker),
			instances:   nil,
		}
		r.servicePools[serviceName] = pool
	}
	// 判断是否需要注册路由
	needRouteRegistration := len(r.routeIndex[serviceName]) == 0
	r.mu.Unlock()

	// 对实例差异进行增删
	// 找出需要新建 invoker 的实例地址
	addrSet := make(map[string]struct{}, len(service.Instances))
	toCreate := make([]*discovery.ServiceInstance, 0)

	pool.mu.RLock()
	existingInvokers := make(map[string]*transcoder.GRPCInvoker, len(pool.invokers))
	maps.Copy(existingInvokers, pool.invokers)
	pool.mu.RUnlock()

	for _, inst := range service.Instances {
		addrSet[inst.Addr] = struct{}{}
		if _, ok := existingInvokers[inst.Addr]; !ok {
			toCreate = append(toCreate, inst)
		}
	}

	// 创建缺失的 invoker
	created := make(map[string]*transcoder.GRPCInvoker)
	var firstFileDescs []*desc.FileDescriptor
	for _, inst := range toCreate {
		invoker, fileDescs, err := transcoder.NewGRPCInvoker(inst.Addr, service.Descriptor)
		if err != nil {
			log.Printf("Warning: failed to create invoker for %s: %v", inst.Addr, err)
			continue
		}
		created[inst.Addr] = invoker
		// 捕获一次描述符用于首次注册路由
		if needRouteRegistration && firstFileDescs == nil && len(fileDescs) > 0 {
			firstFileDescs = fileDescs
		}
	}

	// 合并新建 invoker 移除已下线实例
	toClose := make([]*transcoder.GRPCInvoker, 0)
	pool.mu.Lock()
	// 更新实例列表
	pool.instances = service.Instances
	// 添加新建 invoker
	maps.Copy(pool.invokers, created)
	// 清理下线实例对应的 invoker
	for addr, inv := range pool.invokers {
		if _, alive := addrSet[addr]; !alive {
			toClose = append(toClose, inv)
			delete(pool.invokers, addr)
		}
	}
	pool.mu.Unlock()
	// 在锁外关闭连接 避免阻塞
	for _, inv := range toClose {
		if err := inv.Close(); err != nil {
			log.Printf("Debug: failed to close invoker %v: %v", inv, err)
		}
	}

	// 首次注册该服务的路
	if needRouteRegistration {
		if len(firstFileDescs) == 0 {
			log.Printf("Info: skip route registration for %s due to no descriptors available yet", serviceName)
			return nil
		}

		// 遍历描述符，抽取 HTTP 规则并注册路由
		for _, fileDesc := range firstFileDescs {
			services := fileDesc.GetServices()
			for _, svc := range services {
				methods := svc.GetMethods()
				for _, method := range methods {
					httpRules, err := transcoder.ExtractHTTPRules(method)
					if err != nil {
						log.Printf("Warning: failed to extract HTTP rules for %s: %v", method.GetFullyQualifiedName(), err)
						continue
					}
					for _, httpRule := range httpRules {
						pathKey := normalizePath(httpRule.Method, httpRule.Path)
						// 将检查-插入-索引更新放在同一把锁内 避免竞态
						r.mu.Lock()
						if _, exists := r.pathIndex[pathKey]; exists {
							r.mu.Unlock()
							continue
						}
						// 插入路由 仅在成功时更新索引
						if err := r.routerTree.Insert(
							pathKey,
							&Route{
								ServiceName: serviceName,
								MethodName:  method.GetName(),
								FullMethod:  fmt.Sprintf("%s/%s", svc.GetFullyQualifiedName(), method.GetName()),
								MethodDesc:  method,
								HttpRule:    httpRule,
							},
						); err != nil {
							log.Printf("Warning: failed to insert route for %s %s: %v", serviceName, pathKey, err)
							r.mu.Unlock()
							continue
						}
						log.Printf("Registered route: %s %s -> %s", httpRule.Method, httpRule.Path, fmt.Sprintf("%s/%s", svc.GetFullyQualifiedName(), method.GetName()))
						// 记录索引
						if r.routeIndex[serviceName] == nil {
							r.routeIndex[serviceName] = make(map[string]struct{})
						}
						r.routeIndex[serviceName][pathKey] = struct{}{}
						r.pathIndex[pathKey] = struct{}{}
						r.mu.Unlock()
					}
				}
			}
		}
	}

	return nil
}

// UnRegisterService 注销服务
func (r *HTTPRouter) UnRegisterService(service *discovery.ServiceInfo) error {
	if service == nil || strings.TrimSpace(service.ServiceName) == "" {
		return fmt.Errorf("invalid service info")
	}
	serviceName := service.ServiceName

	// 删除路由树中该服务的所有路由（在同一把锁内进行删除与索引更新）
	r.mu.Lock()
	pathsSet := r.routeIndex[serviceName]
	for pathKey := range pathsSet {
		_ = r.routerTree.Delete(pathKey)
		delete(r.pathIndex, pathKey)
	}

	// 清理索引与服务池引用
	pool := r.servicePools[serviceName]
	delete(r.servicePools, serviceName)
	delete(r.routeIndex, serviceName)
	r.mu.Unlock()

	// 关闭 invokers 并清理实例
	if pool != nil {
		toClose := make([]*transcoder.GRPCInvoker, 0)
		pool.mu.Lock()
		for addr, inv := range pool.invokers {
			toClose = append(toClose, inv)
			delete(pool.invokers, addr)
		}
		pool.instances = nil
		pool.mu.Unlock()
		for _, inv := range toClose {
			if err := inv.Close(); err != nil {
				log.Printf("Debug: failed to close invoker %v: %v", inv, err)
			}
		}
	}
	log.Printf("Remove service: %s", serviceName)
	return nil
}

func (r *HTTPRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, pool := range r.servicePools {
		pool.mu.Lock()
		for _, invoker := range pool.invokers {
			invoker.Close()
		}
		pool.mu.Unlock()
	}
}
