package catalog

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/consul-k8s/helper/coalesce"
	"github.com/hashicorp/go-hclog"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// K8SQuietPeriod is the time to wait for no service changes before syncing.
	K8SQuietPeriod = 1 * time.Second

	// K8SMaxPeriod is the maximum time to wait before forcing a sync, even
	// if there are active changes going on.
	K8SMaxPeriod = 5 * time.Second
)

// Sink is the destination where services are registered.
//
// While in practice we only have one sink (K8S), the interface abstraction
// makes it easy and possible to test the Source in isolation.
type Sink interface {
	// SetServices is called with the services that should be created.
	// The key is the service name and the destination is the external DNS
	// entry to point to.
	SetServices(map[string]string)
}

// K8SSink is a Sink implementation that registers services with Kubernetes.
//
// K8SSink also implements controller.Resource and is meant to run as a K8S
// controller that watches services. This is the primary way that the
// sink should be run.
type K8SSink struct {
	Client    kubernetes.Interface // Client is the K8S API client
	Namespace string               // Namespace is the namespace to sync to
	Log       hclog.Logger         // Logger

	// SyncPeriod is the duration to wait between registering or deregistering
	// services in Kubernetes. This can be fairly short since no work will be
	// done if there are no changes.
	SyncPeriod time.Duration

	lock             sync.Mutex
	sourceServices   map[string]string
	keyToName        map[string]string
	serviceMap       map[string]struct{}
	serviceMapConsul map[string]*apiv1.Service
	triggerCh        chan struct{}
	readyCh          chan struct{}
}

// SetServices implements Sink
func (s *K8SSink) SetServices(svcs map[string]string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.sourceServices = svcs
	s.trigger() // Any service change probably requires syncing
}

// Informer implements the controller.Resource interface.
func (s *K8SSink) Informer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return s.Client.CoreV1().Services(s.namespace()).List(options)
			},

			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return s.Client.CoreV1().Services(s.namespace()).Watch(options)
			},
		},
		&apiv1.Service{},
		0,
		cache.Indexers{},
	)
}

// Upsert implements the controller.Resource interface.
func (s *K8SSink) Upsert(key string, raw interface{}) error {
	// We expect a Service. If it isn't a service then just ignore it.
	service, ok := raw.(*apiv1.Service)
	if !ok {
		s.Log.Warn("upsert got invalid type", "raw", raw)
		return nil
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	// Store all the key to name mappings. We need this because the key
	// is opaque but we want to do all the lookups by service name.
	if s.keyToName == nil {
		s.keyToName = make(map[string]string)
	}
	s.keyToName[key] = service.Name

	if s.serviceMap == nil {
		s.serviceMap = make(map[string]struct{})
	}
	s.serviceMap[service.Name] = struct{}{}

	// If the service is a Consul-sourced service, then keep track of it
	// separately for a quick lookup.
	if service.Labels != nil && service.Labels["consul"] == "true" {
		if s.serviceMapConsul == nil {
			s.serviceMapConsul = make(map[string]*apiv1.Service)
		}

		s.serviceMapConsul[service.Name] = service
		s.trigger() // Always trigger sync
	}

	s.Log.Info("upsert", "key", key)
	return nil
}

// Delete implements the controller.Resource interface.
func (s *K8SSink) Delete(key string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	name, ok := s.keyToName[key]
	if !ok {
		// This is a weird scenario, but in unit tests we've seen this happen
		// in cases where the delete happens very quickly after the create.
		// Just to be sure, lets trigger a sync. This is cheap cause it'll
		// do nothing if there is no work to be done.
		s.trigger()
		return nil
	}

	delete(s.keyToName, key)
	delete(s.serviceMap, name)
	delete(s.serviceMapConsul, name)

	// If the service that is deleted is part of Consul services, then
	// we need to trigger a sync to recreate it.
	if _, ok := s.sourceServices[name]; ok {
		s.trigger()
	}

	s.Log.Info("delete", "key", key, "name", name)
	return nil
}

// Run implements the controller.Backgrounder interface.
func (s *K8SSink) Run(ch <-chan struct{}) {
	s.Log.Info("starting runner for syncing")

	// Initialize the trigger channel. We send an initial message so that
	// our loop below runs immediately.
	s.lock.Lock()
	var triggerCh chan struct{}
	if s.triggerCh == nil {
		triggerCh = make(chan struct{}, 1)
		triggerCh <- struct{}{}
		s.triggerCh = triggerCh
	}
	s.lock.Unlock()

	for {
		select {
		case <-ch:
			return
		case <-triggerCh:
			// Coalesce to prevent lots of API calls during churn periods.
			coalesce.Coalesce(context.Background(),
				K8SQuietPeriod, K8SMaxPeriod,
				func(ctx context.Context) {
					select {
					case <-triggerCh:
					case <-ctx.Done():
					}
				})
		}

		s.lock.Lock()
		create, update, delete := s.crudList()
		s.lock.Unlock()
		s.Log.Debug("sync triggered", "create", len(create), "update", len(update), "delete", len(delete))

		svcClient := s.Client.CoreV1().Services(s.namespace())
		for _, name := range delete {
			if err := svcClient.Delete(name, nil); err != nil {
				s.Log.Warn("error deleting service", "name", name, "error", err)
			}
		}

		for _, svc := range update {
			_, err := svcClient.Update(svc)
			if err != nil {
				s.Log.Warn("error updating service", "name", svc.Name, "error", err)
			}
		}

		for _, svc := range create {
			_, err := svcClient.Create(svc)
			if err != nil {
				s.Log.Warn("error creating service", "name", svc.Name, "error", err)
			}
		}
	}
}

// crudList returns the services to create, update, and delete (respectively).
func (s *K8SSink) crudList() ([]*apiv1.Service, []*apiv1.Service, []string) {
	var create, update []*apiv1.Service
	var delete []string

	// Determine what needs to be created or updated
	for k, v := range s.sourceServices {
		// If this is an already registered service, then update it
		if s.serviceMapConsul != nil {
			if svc, ok := s.serviceMapConsul[k]; ok && svc.Spec.ExternalName != v {
				svc.Spec = apiv1.ServiceSpec{
					Type:         apiv1.ServiceTypeExternalName,
					ExternalName: v,
				}

				update = append(update, svc)
				continue
			}
		}

		// If this is a registered K8S service, ignore.
		if _, ok := s.serviceMap[k]; ok {
			s.Log.Warn("service already registered in K8S, not registering", "name", k)
			continue
		}

		// Register!
		create = append(create, &apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:   k,
				Labels: map[string]string{"consul": "true"},
				Annotations: map[string]string{
					// Ensure we don't sync the service back to Consul
					"consul.hashicorp.com/service-sync": "false",
				},
			},

			Spec: apiv1.ServiceSpec{
				Type:         apiv1.ServiceTypeExternalName,
				ExternalName: v,
			},
		})
	}

	// Determine what needs to be deleted
	for k, _ := range s.serviceMapConsul {
		if _, ok := s.sourceServices[k]; !ok {
			delete = append(delete, k)
		}
	}

	return create, update, delete
}

// namespace returns the K8S namespace to setup the resource watchers in.
func (s *K8SSink) namespace() string {
	if s.Namespace != "" {
		return s.Namespace
	}

	// Default to the default namespace. This should not be "all" since we
	// want a specific namespace to watch and write to.
	return metav1.NamespaceDefault
}

// trigger will notify a sync should occur. lock must be held.
//
// This is not synchronous and does not guarantee a sync will happen. This
// just sends a notification that a sync is likely necessary.
func (s *K8SSink) trigger() {
	if s.triggerCh != nil {
		// Non-blocking send. This is okay because we always buffer triggerCh
		// to one. So if this blocks it means that a message is already waiting
		// which is equivalent to us sending the trigger. No information loss!
		select {
		case s.triggerCh <- struct{}{}:
		default:
		}
	}
}
