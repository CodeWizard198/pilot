package router

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// NodeType 节点类型
type NodeType uint8

const (
	Static   NodeType = iota // 静态
	Param                    // 参数
	Wildcard                 // 通配符
)

// node Radix树的节点
type node[T any] struct {
	segment string
	typeOf  NodeType
	// 静态子节点
	children map[string]*node[T]
	// 参数子节点(如 :id)
	paramName  string
	paramChild *node[T]
	// 通配符子节点(如 *filepath 终止匹配)
	wildName  string
	wildChild *node[T]

	// 关联值
	valueMu  sync.RWMutex
	value    *T
	fullPath string
}

// RouteTree 路由树 支持并发读写
// 结构性修改使用 treeMu 锁
// 读使用 atomic.Value 的根指针实现 RCU 风格无锁读取
type RouteTree[T any] struct {
	root   atomic.Value // *node[T]
	freeze sync.RWMutex // 树级锁：写锁保护结构修,读锁用于同步快照
}

// NewRouteTree 创建路由
func NewRouteTree[T any]() *RouteTree[T] {
	var r RouteTree[T]
	root := &node[T]{
		segment:  "",
		typeOf:   Static,
		children: make(map[string]*node[T]),
	}
	r.root.Store(root)
	return &r
}

// Insert 插入路径与值，支持节点分裂与冲突检测
func (r *RouteTree[T]) Insert(path string, val T) error {
	if path == "" || path[0] != '/' {
		return errors.New("path must start with '/'")
	}
	segs := splitPath(path)

	r.freeze.Lock()
	defer r.freeze.Unlock()

	// 使用当前快照进行结构修改
	root := r.root.Load().(*node[T])
	cur := root
	for i := range segs {
		seg := segs[i]
		// 处理重复斜杠
		if len(seg) == 0 {
			continue
		}

		// 参数节点
		if seg[0] == ':' {
			name := seg[1:]
			if name == "" {
				return errors.New("param name cannot be empty")
			}
			// 通配符与参数冲突
			if cur.wildChild != nil {
				return errors.New("conflict: wildcard and param at same level")
			}
			if cur.paramChild == nil {
				cur.paramChild = &node[T]{
					segment:   seg,
					typeOf:    Param,
					children:  make(map[string]*node[T]),
					paramName: name,
				}
			}
			if cur.paramChild.paramName != name {
				return errors.New("conflict: different param names at same level")
			}
			cur = cur.paramChild
			continue
		}

		// 通配符节点 必须为最后一段
		if seg[0] == '*' {
			name := seg[1:]
			if name == "" {
				return errors.New("wildcard name cannot be empty")
			}
			if i != len(segs)-1 {
				return errors.New("wildcard must be the last segment")
			}
			// 与静态/参数冲突
			if len(cur.children) > 0 || cur.paramChild != nil {
				return errors.New("conflict: wildcard cannot coexist with other children")
			}
			if cur.wildChild == nil {
				cur.wildChild = &node[T]{
					segment:  seg,
					typeOf:   Wildcard,
					children: make(map[string]*node[T]),
					wildName: name,
				}
			}
			cur = cur.wildChild
			continue
		}

		// 静态段
		child := cur.children[seg]
		if child == nil {
			child = &node[T]{
				segment:  seg,
				typeOf:   Static,
				children: make(map[string]*node[T]),
			}
			cur.children[seg] = child
		}
		cur = child
	}

	// 赋值(节点级锁)
	cur.valueMu.Lock()
	defer cur.valueMu.Unlock()
	if cur.value != nil {
		// 已存在值,视为更新
		*cur.value = val
		cur.fullPath = path
	} else {
		v := val
		cur.value = &v
		cur.fullPath = path
	}

	// 刷新快照(RCU)
	r.root.Store(root)
	return nil
}

// Lookup 查找路径 返回值与参数映射
func (r *RouteTree[T]) Lookup(path string) (T, map[string]string, bool) {
	var zero T
	if path == "" || path[0] != '/' {
		return zero, nil, false
	}
	segs := splitPath(path)
	params := make(map[string]string)

	r.freeze.RLock()
	defer r.freeze.RUnlock()

	// 在读锁保护下读取快照并遍历
	root := r.root.Load().(*node[T])
	cur := root
	for i := range segs {
		seg := segs[i]
		if seg == "" {
			continue
		}
		// 优先级：静态 > 参数 > 通配符
		if child := cur.children[seg]; child != nil {
			cur = child
			continue
		}
		if cur.paramChild != nil {
			params[cur.paramChild.paramName] = seg
			cur = cur.paramChild
			continue
		}
		if cur.wildChild != nil {
			remain := strings.Join(segs[i:], "/")
			params[cur.wildChild.wildName] = remain
			cur = cur.wildChild
			break
		}
		return zero, nil, false
	}

	cur.valueMu.RLock()
	defer cur.valueMu.RUnlock()
	if cur.value == nil {
		return zero, nil, false
	}
	return *cur.value, params, true
}

// Update 原子更新指定路径的值(如果存在)
func (r *RouteTree[T]) Update(path string, updater func(old T) (T, bool)) bool {
	if path == "" || path[0] != '/' {
		return false
	}
	segs := splitPath(path)

	r.freeze.RLock()
	root := r.root.Load().(*node[T])
	cur := root
	found := true
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if seg == "" {
			continue
		}
		// 匹配优先级：静态 > 参数 > 通配符
		if child := cur.children[seg]; child != nil {
			cur = child
			continue
		}
		if cur.paramChild != nil {
			cur = cur.paramChild
			continue
		}
		if cur.wildChild != nil {
			cur = cur.wildChild
			break
		}
		found = false
		break
	}
	r.freeze.RUnlock()
	if !found {
		return false
	}

	cur.valueMu.Lock()
	defer cur.valueMu.Unlock()
	if cur.value == nil {
		return false
	}
	newVal, apply := updater(*cur.value)
	if !apply {
		return false
	}
	*cur.value = newVal
	return true
}

// Delete 删除路径并做结构压缩
func (r *RouteTree[T]) Delete(path string) bool {
	if path == "" || path[0] != '/' {
		return false
	}
	segs := splitPath(path)

	r.freeze.Lock()
	defer r.freeze.Unlock()

	root := r.root.Load().(*node[T])
	cur := root
	stack := make([]*node[T], 0, len(segs)+1)
	keys := make([]string, 0, len(segs)+1)
	stack = append(stack, cur)
	keys = append(keys, "")

	for i := range segs {
		seg := segs[i]
		if seg == "" {
			continue
		}
		if child := cur.children[seg]; child != nil {
			cur = child
			stack = append(stack, cur)
			keys = append(keys, seg)
			continue
		}
		if cur.paramChild != nil {
			cur = cur.paramChild
			stack = append(stack, cur)
			keys = append(keys, ":"+cur.paramName)
			continue
		}
		if cur.wildChild != nil {
			cur = cur.wildChild
			stack = append(stack, cur)
			keys = append(keys, "*"+cur.wildName)
			break
		}
		return false
	}

	cur.valueMu.Lock()
	had := cur.value != nil
	cur.value = nil
	cur.fullPath = ""
	cur.valueMu.Unlock()
	if !had {
		return false
	}

	// 结构压缩: 自叶向上移除无值且无子节点的节点
	for i := len(stack) - 1; i > 0; i-- {
		n := stack[i]
		p := stack[i-1]
		if n.value == nil && len(n.children) == 0 && n.paramChild == nil && n.wildChild == nil {
			key := keys[i]
			if strings.HasPrefix(key, ":") {
				p.paramChild = nil
			} else if strings.HasPrefix(key, "*") {
				p.wildChild = nil
			} else {
				delete(p.children, key)
			}
		}
	}

	// 刷新快照
	r.root.Store(root)
	return true
}

// splitPath 将路径拆分为段
func splitPath(path string) []string {
	// 去除重复斜杠，并按 '/' 分割
	// 在此之前剥离查询字符串与片段
	p := path
	if p == "" {
		return []string{""}
	}
	p = strings.Trim(p, " ")
	// 先去掉查询与片段
	if idx := strings.IndexByte(p, '?'); idx >= 0 {
		p = p[:idx]
	}
	if idx := strings.IndexByte(p, '#'); idx >= 0 {
		p = p[:idx]
	}
	// 标准化连续斜杠:
	// 循环替换确保任意长度的多个斜杠被归一
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p == "/" {
		return []string{""}
	}
	if len(p) > 0 && p[0] != '/' {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	// 删除第一项空字符串
	if len(segs) > 0 && segs[0] == "" {
		segs = segs[1:]
	}
	return segs
}
