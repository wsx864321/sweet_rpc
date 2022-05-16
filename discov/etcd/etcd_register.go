package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/wsx864321/sweet_rpc/discov"
)

const KeyPrefix = "/sweet_rpc/service/register/"

// Register ...
type Register struct {
	Options
	cli                 *clientv3.Client
	serviceRegisterCh   chan *discov.Service
	serviceUnRegisterCh chan *discov.Service
	lock                sync.RWMutex
	downServices        atomic.Value
	registerServices    map[string]*registerService
}

type registerService struct {
	service      *discov.Service
	leaseID      clientv3.LeaseID
	isRegistered bool
	keepAliveCh  <-chan *clientv3.LeaseKeepAliveResponse
}

// NewETCDRegister ...
func NewETCDRegister(opts ...Option) discov.Discovery {
	opt := defaultOption
	for _, o := range opts {
		o(&opt)
	}

	return &Register{
		Options:           opt,
		serviceRegisterCh: make(chan *discov.Service),
	}
}

// Init 初始化
func (r *Register) Init(ctx context.Context) error {
	var err error
	r.cli, err = clientv3.New(
		clientv3.Config{
			Endpoints:   r.endpoints,
			DialTimeout: r.dialTimeout,
		})

	if err != nil {
		return err
	}

	return nil
}

func (r *Register) run() {
	for {
		select {
		case service := <-r.serviceRegisterCh:
			// 这个地方不去重，如果上游代码有问题，可能会导致出现问题，先简单做吧，todo
			if _, ok := r.registerServices[service.Name]; ok {
				r.registerServices[service.Name].service.Endpoints = append(r.registerServices[service.Name].service.Endpoints, service.Endpoints...)
				r.registerServices[service.Name].isRegistered = false // 重新上报到etcd
			} else {
				r.registerServices[service.Name] = &registerService{
					service:      service,
					isRegistered: false,
				}
			}
		case service := <-r.serviceUnRegisterCh:
			if _, ok := r.registerServices[service.Name]; !ok {
				r.logger.Errorf(context.TODO(), "UnRegisterService err, service %v was not registered", service.Name)
				continue
			}
			r.unRegisterService(context.TODO(), service)
		default:
			r.registerServiceOrKeepAlive(context.TODO())
			time.Sleep(r.registerServiceOrKeepAliveInterval)
		}
	}
}

func (r *Register) registerServiceOrKeepAlive(ctx context.Context) {
	for _, service := range r.registerServices {
		if !service.isRegistered {
			r.registerService(ctx, service)
			r.registerServices[service.service.Name].isRegistered = true
		} else {
			r.KeepAlive(ctx, service)
		}
	}
}

func (r *Register) registerService(ctx context.Context, service *registerService) {
	leaseGrantResp, err := r.cli.Grant(ctx, r.keepAliveInterval)
	if err != nil {
		r.logger.Errorf(ctx, "register service grant,err:%v", err)
		return
	}
	service.leaseID = leaseGrantResp.ID

	for _, endpoint := range service.service.Endpoints {
		tmp := discov.Service{
			Name:      service.service.Name,
			Endpoints: []*discov.Endpoint{endpoint},
		}

		key := r.getEtcdRegisterKey(service.service.Name, endpoint.IP, endpoint.Port)
		raw, err := json.Marshal(tmp)
		if err != nil {
			r.logger.Errorf(ctx, "register service err,err:%v, register data:%v", err, string(raw))
			continue
		}
		_, err = r.cli.Put(ctx, key, string(raw), clientv3.WithLease(leaseGrantResp.ID))
		if err != nil {
			r.logger.Errorf(ctx, "register service err,err:%v, register data:%v", err, string(raw))
			continue
		}

	}

	keepAliveCh, err := r.cli.KeepAlive(ctx, leaseGrantResp.ID)
	if err != nil {
		r.logger.Errorf(ctx, "register service keepalive,err:%v", err)
		return
	}

	service.keepAliveCh = keepAliveCh
	service.isRegistered = true

}

func (r *Register) unRegisterService(ctx context.Context, service *discov.Service) {
	endpoints := make([]*discov.Endpoint, 0)
	for _, endpoint := range r.registerServices[service.Name].service.Endpoints {
		var isRemove bool
		for _, unRegisterEndpoint := range service.Endpoints {
			if endpoint.IP == unRegisterEndpoint.IP && endpoint.Port == unRegisterEndpoint.Port {
				_, err := r.cli.Delete(context.TODO(), r.getEtcdRegisterKey(service.Name, endpoint.IP, endpoint.Port))
				if err != nil {
					r.logger.Errorf(ctx, "UnRegisterService etcd del err, service %v was not registered", service.Name)
				}
				isRemove = true
				break
			}
		}

		if !isRemove {
			endpoints = append(endpoints, endpoint)
		}
	}

	if len(endpoints) == 0 {
		delete(r.registerServices, service.Name)
	} else {
		r.registerServices[service.Name].service.Endpoints = endpoints
	}
}

func (r *Register) KeepAlive(ctx context.Context, service *registerService) {
	for {
		select {
		case <-service.keepAliveCh:
		default:
			return
		}
	}
}

func (r *Register) Name() string {
	return "etcd"
}

func (r *Register) Register(ctx context.Context, service *discov.Service) {
	r.serviceRegisterCh <- service
}

func (r *Register) UnRegister(ctx context.Context, service *discov.Service) {
	r.serviceUnRegisterCh <- service
}

func (r *Register) GetService(ctx context.Context, name string) *discov.Service {
	allServices := r.downServices.Load().(map[string]*discov.Service)
	if val, ok := allServices[name]; ok {
		return val
	}

	// 防止并发获取service导致cache中的数据混乱
	r.lock.Lock()
	defer r.lock.Unlock()

	key := r.getEtcdRegisterPrefixKey(name)
	getResp, _ := r.cli.Get(ctx, key, clientv3.WithPrefix())
	service := &discov.Service{
		Name:      name,
		Endpoints: make([]*discov.Endpoint, 0),
	}

	for _, item := range getResp.Kvs {
		var endpoint discov.Endpoint
		if err := json.Unmarshal(item.Value, &endpoint); err != nil {
			continue
		}

		service.Endpoints = append(service.Endpoints, &endpoint)
	}

	allServices[name] = service
	r.downServices.Store(allServices)

	go func() {
		r.watch(ctx, key, getResp.Header.Revision+1)
	}()

	return service
}

func (r *Register) watch(ctx context.Context, key string, leaseID int64) {
	rch := r.cli.Watch(ctx, key, clientv3.WithLease(clientv3.LeaseID(leaseID)))
	for n := range rch {
		for _, ev := range n.Events {
			switch ev.Type {
			case mvccpb.PUT:
				var service discov.Service
				if err := json.Unmarshal(ev.Kv.Value, &service); err != nil {
					continue
				}
				r.updateDownService(&service)
			case mvccpb.DELETE:
				var service discov.Service
				if err := json.Unmarshal(ev.Kv.Value, &service); err != nil {
					continue
				}
				r.delDownService(&service)
			}
		}
	}
}

func (r *Register) updateDownService(service *discov.Service) {
	downServices := r.downServices.Load().(map[string]*discov.Service)
	if _, ok := downServices[service.Name]; !ok {
		return
	}

	for _, newAddEndpoint := range service.Endpoints {
		var isExist bool
		for idx, endpoint := range downServices[service.Name].Endpoints {
			if newAddEndpoint.IP == endpoint.IP && newAddEndpoint.Port == endpoint.Port {
				downServices[service.Name].Endpoints[idx] = newAddEndpoint
				isExist = true
				break
			}
		}

		if !isExist {
			downServices[service.Name].Endpoints = append(downServices[service.Name].Endpoints, newAddEndpoint)
		}
	}

	r.downServices.Store(downServices)
}

func (r *Register) delDownService(service *discov.Service) {
	downServices := r.downServices.Load().(map[string]*discov.Service)
	if _, ok := downServices[service.Name]; !ok {
		return
	}

	endpoints := make([]*discov.Endpoint, 0)
	for _, endpoint := range downServices[service.Name].Endpoints {
		var isRemove bool
		for _, delEndpoint := range service.Endpoints {
			if delEndpoint.IP == endpoint.IP && delEndpoint.Port == endpoint.Port {
				isRemove = true
				break
			}
		}

		if !isRemove {
			endpoints = append(endpoints, endpoint)
		}
	}

	downServices[service.Name].Endpoints = endpoints
	r.downServices.Store(downServices)
}

func (r *Register) getEtcdRegisterKey(name, ip string, port int) string {
	return fmt.Sprintf(KeyPrefix+"%v/%v/%v", name, ip, port)
}

func (r *Register) getEtcdRegisterPrefixKey(name string) string {
	return fmt.Sprintf(KeyPrefix+"%v", name)
}