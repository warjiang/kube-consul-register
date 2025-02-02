package services

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/warjiang/kube-consul-register/config"
	"github.com/warjiang/kube-consul-register/consul"
	"github.com/warjiang/kube-consul-register/metrics"
	"github.com/warjiang/kube-consul-register/utils"

	"k8s.io/client-go/kubernetes"
	//"k8s.io/client-go/pkg/api/v1"
	//"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/tools/cache"

	consulapi "github.com/hashicorp/consul/api"
)

// These are valid annotations names which are take into account.
// "ConsulRegisterEnabledAnnotation" is a name of annotation key for `enabled` option.
const (
	ConsulRegisterEnabledAnnotation                string = "consul.register/enabled"
	ConsulRegisterServiceNameAnnotation            string = "consul.register/service.name"
	ConsulRegisterServiceTags                      string = "consul.register/service.tags"
	ConsulRegisterServiceHealthIntervalAnnotation  string = "consul.register/service.health.interval"
	ConsulRegisterServiceHealthTimeoutAnnotation   string = "consul.register/service.health.timeout"
	ConsulRegisterServiceHealthCheckPathAnnotation string = "consul.register/service.health.path"
	ConsulRegisterServiceHealthHostAnnotation      string = "consul.register/service.health.host"
	ConsulRegisterServiceHealthPortAnnotation      string = "consul.register/service.health.port"
	ConsulRegisterServiceHealthTTLAnnotation       string = "consul.register/service.health.ttl"
	ConsulRegisterServiceHealthTCPAnnotation       string = "consul.register/service.health.tcp"
)

var (
	allAddedServices = make(map[string]bool)

	consulAgents map[string]*consul.Adapter
)

// Controller describes the attributes that are uses by Controller
type Controller struct {
	clientset      *kubernetes.Clientset
	consulInstance consul.Adapter
	cfg            *config.Config
	namespace      string
	mutex          *sync.Mutex
}

// New creates an instance of controller
func New(clientset *kubernetes.Clientset, consulInstance consul.Adapter, cfg *config.Config, namespace string) FactoryAdapter {
	return &Controller{
		clientset:      clientset,
		consulInstance: consulInstance,
		cfg:            cfg,
		namespace:      namespace,
		mutex:          &sync.Mutex{}}
}

func (c *Controller) cacheConsulAgent() (map[string]*consul.Adapter, error) {
	consulAgents = make(map[string]*consul.Adapter)

	ctx := context.TODO()
	//Cache Consul's Agents
	if c.cfg.Controller.RegisterMode == config.RegisterSingleMode {
		consulAgent := c.consulInstance.New(c.cfg, "", "")
		consulAgents[c.cfg.Controller.ConsulAddress] = consulAgent

	} else if c.cfg.Controller.RegisterMode == config.RegisterNodeMode {
		nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: c.cfg.Controller.ConsulNodeSelector,
		})
		if err != nil {
			return consulAgents, err
		}
		// !! should mount /etc/hosts to pod
		// may be the name of node.ObjectMeta.Name is the hostname of node, not real ip address
		for _, node := range nodes.Items {
			consulInstance := consul.Adapter{}
			_ = utils.GetHostIP(node)
			consulAgent := consulInstance.New(c.cfg, utils.GetHostIP(node), "")
			consulAgents[node.ObjectMeta.Name] = consulAgent
		}
	} else if c.cfg.Controller.RegisterMode == config.RegisterPodMode {
		pods, err := c.clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
			LabelSelector: c.cfg.Controller.PodLabelSelector,
		})
		if err != nil {
			return consulAgents, err
		}
		for _, pod := range pods.Items {
			consulInstance := consul.Adapter{}
			consulAgent := consulInstance.New(c.cfg, "", pod.Status.HostIP)
			consulAgents[pod.Status.HostIP] = consulAgent
		}
	}

	return consulAgents, nil
}

// Clean checks Consul services and remove them if service does not appear in K8S cluster
func (c *Controller) Clean() error {
	timer := prometheus.NewTimer(metrics.FuncDuration.WithLabelValues("clean"))
	defer timer.ObserveDuration()

	var err error

	c.mutex.Lock()

	consulAgents, err = c.cacheConsulAgent()
	if err != nil {
		c.mutex.Unlock()
		return fmt.Errorf("Can't cache Consul' Agents: %s", err)
	}

	// Get list of added Consul' services
	// addedConsulServices map[string]string serviceConsulID:consul_agent_hostname
	// registeredConsulServices map[string][]string UID:serviceConsulID
	addedConsulServices, registeredConsulServices, err := c.getAddedConsulServices()
	if err != nil {
		c.mutex.Unlock()
		return err
	}
	glog.V(3).Infof("Added services: %#v", addedConsulServices)

	allServices, err := c.clientset.CoreV1().Services(c.namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		c.mutex.Unlock()
		return err
	}

	var currentAddedServices = make(map[string]string)
	for _, service := range allServices.Items {
		if !isRegisterEnabled(&service) {
			continue
		}
		currentAddedServices[string(service.ObjectMeta.UID)] = service.ObjectMeta.Name
	}

	for uid, serviceConsulID := range registeredConsulServices {
		if name, ok := currentAddedServices[uid]; !ok {
			for _, serviceID := range serviceConsulID {
				consulAgent := consulAgents[addedConsulServices[serviceID]]
				consulService := &consulapi.AgentServiceRegistration{
					ID: serviceID,
				}

				err = consulAgent.Deregister(consulService)
				if err != nil {
					glog.Errorf("Cannot deregister service in Consul: %s", err)
					metrics.ConsulFailure.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
				} else {
					delete(allAddedServices, serviceID)
					glog.Infof("Service %s has been deregistered in Consul with ID: %s", name, serviceID)
					metrics.ConsulSuccess.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
				}
			}
		}
	}

	c.mutex.Unlock()
	return nil
}

// Sync synchronizes services between Consul and K8S cluster
func (c *Controller) Sync() error {
	timer := prometheus.NewTimer(metrics.FuncDuration.WithLabelValues("sync"))
	defer timer.ObserveDuration()

	var err error

	c.mutex.Lock()

	consulAgents, err = c.cacheConsulAgent()
	if err != nil {
		c.mutex.Unlock()
		return fmt.Errorf("Can't cache Consul' Agents: %s", err)
	}
	glog.V(2).Infof("Agents: %#v", consulAgents)

	// Get list of added Consul' services
	// addedConsulServices map[string]string serviceConsulID:consul_agent_hostname
	// registeredConsulServices map[string][]string UID:serviceConsulID
	addedConsulServices, registeredConsulServices, err := c.getAddedConsulServices()
	if err != nil {
		c.mutex.Unlock()
		return err
	}
	glog.V(3).Infof("Added services: %#v", addedConsulServices)

	allServices, err := c.clientset.CoreV1().Services(c.namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		c.mutex.Unlock()
		return err
	}

	for _, service := range allServices.Items {
		if !isRegisterEnabled(&service) {
			continue
		}

		// Check if service has already added to Consul
		if consulServices, ok := registeredConsulServices[string(service.ObjectMeta.UID)]; ok {
			for _, serviceConsulID := range consulServices {
				if _, ok := addedConsulServices[serviceConsulID]; !ok {
					err := c.eventAddFunc(&service)
					if err != nil {
						c.mutex.Unlock()
						return err
					}
				}
			}
		} else {
			err := c.eventAddFunc(&service)
			if err != nil {
				c.mutex.Unlock()
				return err
			}
		}
	}
	c.mutex.Unlock()
	return nil
}

// nodeDelete deletes service after node deletion
func (c *Controller) nodeDelete(obj interface{}) error {
	timer := prometheus.NewTimer(metrics.FuncDuration.WithLabelValues("sync"))
	defer timer.ObserveDuration()

	var err error

	c.mutex.Lock()
	allAddedServices = make(map[string]bool)

	consulAgents, err = c.cacheConsulAgent()
	if err != nil {
		c.mutex.Unlock()
		return fmt.Errorf("Can't cache Consul' Agents: %s", err)
	}
	glog.V(2).Infof("Agents: %#v", consulAgents)

	// Get list of added Consul' services
	// addedConsulServices map[string]string serviceConsulID:consul_agent_hostname
	// registeredConsulServices map[string][]string UID:serviceConsulID
	addedConsulServices, _, err := c.getAddedConsulServices()
	if err != nil {
		c.mutex.Unlock()
		return err
	}
	glog.V(3).Infof("Added services: %#v", addedConsulServices)

	for serviceConsulID, consulAgentHostname := range addedConsulServices {
		for _, address := range obj.(*v1.Node).Status.Addresses {
			if strings.Contains(serviceConsulID, "-"+address.Address+"-") {
				consulAgent := consulAgents[consulAgentHostname]
				consulService := &consulapi.AgentServiceRegistration{
					ID: serviceConsulID,
				}

				err = consulAgent.Deregister(consulService)
				if err != nil {
					glog.Errorf("Cannot deregister service in Consul: %s", err)
					metrics.ConsulFailure.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
				} else {
					delete(allAddedServices, serviceConsulID)
					glog.Infof("Service has been deregistered in Consul with ID: %s", serviceConsulID)
					metrics.ConsulSuccess.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
				}
			}
		}
	}

	c.mutex.Unlock()
	return nil
}

// Watch watches events in K8S cluster
func (c *Controller) Watch() {
	go c.watchNodes()
	go c.watchServices()
}

func (c *Controller) watchNodes() {
	watchlist := cache.NewListWatchFromClient(c.clientset.CoreV1().RESTClient(), "nodes", c.namespace,
		fields.Everything())
	_, controller := cache.NewInformer(
		watchlist,
		&v1.Node{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				c.mutex.Lock()
				glog.Info("Add node.")
				allServices, err := c.clientset.CoreV1().Services(c.namespace).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					c.mutex.Unlock()
				}

				for _, service := range allServices.Items {
					if !isRegisterEnabled(&service) {
						continue
					}

					if err := c.eventAddFunc(&service); err != nil {
						glog.Errorf("Failed to add node: %s", err)
					}
				}
				c.mutex.Unlock()
			},
			DeleteFunc: func(obj interface{}) {
				glog.Info("Delete node. ")
				err := c.nodeDelete(obj)
				if err != nil {
					glog.Error(err)
				}
			},
		},
	)

	stop := make(chan struct{})
	controller.Run(stop)
}

func (c *Controller) watchServices() {
	watchlist := cache.NewListWatchFromClient(c.clientset.CoreV1().RESTClient(), "services", c.namespace,
		fields.Everything())
	_, controller := cache.NewInformer(
		watchlist,
		&v1.Service{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if !isRegisterEnabled(obj) {
					return
				}

				c.mutex.Lock()
				if err := c.eventAddFunc(obj); err != nil {
					glog.Errorf("Failed to add services: %s", err)
				}
				c.mutex.Unlock()
			},
			DeleteFunc: func(obj interface{}) {
				timer := prometheus.NewTimer(metrics.FuncDuration.WithLabelValues("delete"))
				defer timer.ObserveDuration()
				if !isRegisterEnabled(obj) {
					return
				}

				c.mutex.Lock()
				if err := c.eventDeleteFunc(obj); err != nil {
					glog.Errorf("Failed to delete services: %s", err)
				}
				c.mutex.Unlock()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				timer := prometheus.NewTimer(metrics.FuncDuration.WithLabelValues("update"))
				defer timer.ObserveDuration()
				if !isRegisterEnabled(newObj) {
					// Deregister the service on update if disabled
					c.mutex.Lock()
					if err := c.eventDeleteFunc(newObj); err != nil {
						glog.Errorf("Failed to delete services during update: %s", err)
					}
					c.mutex.Unlock()
				} else {
					c.mutex.Lock()
					if err := c.eventAddFunc(newObj); err != nil {
						glog.Errorf("Failed to add services during update: %s", err)
					}
					c.mutex.Unlock()
				}
			},
		},
	)

	stop := make(chan struct{})
	controller.Run(stop)
}

// getAddedConsulServices returns the list of added Consul Services
func (c *Controller) getAddedConsulServices() (map[string]string, map[string][]string, error) {
	var addedServices = make(map[string]string)
	var registeredConsulServices = make(map[string][]string)

	// Make list of Consul's services
	for consulAgentID, consulAgent := range consulAgents {
		services, err := consulAgent.Services()
		if err != nil {
			glog.Errorf("Can't get services from Consul Agent, register mode=%s: %s", c.cfg.Controller.RegisterMode, err)
		} else {
			glog.V(3).Infof("agent: %#v, services: %#v", consulAgentID, services)
			for _, service := range services {
				if utils.CheckK8sTag(service.Tags, c.cfg.Controller.K8sTag) {
					addedServices[service.ID] = consulAgentID

					uid := utils.GetConsulServiceTag(service.Tags, "uid")
					if value, ok := registeredConsulServices[uid]; ok {
						registeredConsulServices[uid] = append(value, service.ID)
					} else {
						registeredConsulServices[uid] = []string{service.ID}
					}
				}
			}
		}
	}
	return addedServices, registeredConsulServices, nil
}

func (c *Controller) eventAddFunc(obj interface{}) error {
	if !isRegisterEnabled(obj) {
		return nil
	}

	var nodesIPs []string
	var ports []int32
	var err error

	switch serviceType := obj.(*v1.Service).Spec.Type; serviceType {
	case v1.ServiceTypeNodePort:
		// Check if ExternalIPs is empty
		if len(obj.(*v1.Service).Spec.ExternalIPs) > 0 {
			nodesIPs = obj.(*v1.Service).Spec.ExternalIPs
		} else {
			nodesIPs, err = c.getNodesIPs()
			if err != nil {
				return err
			}
		}
		for _, port := range obj.(*v1.Service).Spec.Ports {
			if port.Protocol == v1.ProtocolTCP {
				ports = append(ports, port.NodePort)
			}
		}

		// Now is time to add service to Consul
		for _, nodeAddress := range nodesIPs {
			for _, port := range ports {
				// Add to Consul
				service, err := c.createConsulService(obj.(*v1.Service), nodeAddress, port)
				if err != nil {
					glog.Errorf("Cannot create Consul service: %s", err)
					continue
				}
				// Check if service's already added
				if _, ok := allAddedServices[service.ID]; ok {
					glog.V(3).Infof("Service %s has already registered in Consul", service.ID)
					continue
				}

				consulAgent := c.consulInstance.New(c.cfg, nodeAddress, "")
				err = consulAgent.Register(service)
				if err != nil {
					glog.Errorf("Cannot register service in Consul: %s", err)
					metrics.ConsulFailure.WithLabelValues("register", consulAgent.Config.Address).Inc()
				} else {
					allAddedServices[service.ID] = true
					glog.Infof("Service %s has been registered in Consul with ID: %s", obj.(*v1.Service).ObjectMeta.Name, service.ID)
					metrics.ConsulSuccess.WithLabelValues("register", consulAgent.Config.Address).Inc()
				}
			}
		}
	case v1.ServiceTypeClusterIP:
		// Check if ExternalIPs is empty
		if len(obj.(*v1.Service).Spec.ExternalIPs) > 0 {
			nodesIPs = obj.(*v1.Service).Spec.ExternalIPs
		} else {
			return nil
		}
		for _, port := range obj.(*v1.Service).Spec.Ports {
			if port.Protocol == v1.ProtocolTCP {
				ports = append(ports, port.NodePort)
			}
		}

		// Now is time to add service to Consul
		for _, nodeAddress := range nodesIPs {
			for _, port := range ports {
				// Add to Consul
				service, err := c.createConsulService(obj.(*v1.Service), nodeAddress, port)
				if err != nil {
					glog.Errorf("Cannot create Consul service: %s", err)
					continue
				}
				// Check if service's already added
				if _, ok := allAddedServices[service.ID]; ok {
					glog.V(3).Infof("Service %s has already registered in Consul", service.ID)
					continue
				}

				consulAgent := c.consulInstance.New(c.cfg, nodeAddress, "")
				err = consulAgent.Register(service)
				if err != nil {
					glog.Errorf("Cannot register service in Consul: %s", err)
					metrics.ConsulFailure.WithLabelValues("register", consulAgent.Config.Address).Inc()
				} else {
					allAddedServices[service.ID] = true
					glog.Infof("Service %s has been registered in Consul with ID: %s", obj.(*v1.Service).ObjectMeta.Name, service.ID)
					metrics.ConsulSuccess.WithLabelValues("register", consulAgent.Config.Address).Inc()
				}
			}
		}
	}
	return nil
}

func (c *Controller) getNodesIPs() ([]string, error) {
	var listOptions metav1.ListOptions
	if c.cfg.Controller.RegisterMode == config.RegisterNodeMode {
		listOptions.LabelSelector = c.cfg.Controller.ConsulNodeSelector
	}
	nodes, err := c.clientset.CoreV1().Nodes().List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}

	var addresses []string
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			switch addressType := address.Type; addressType {
			case v1.NodeExternalIP:
				addresses = append(addresses, address.Address)
			case v1.NodeInternalIP:
				addresses = append(addresses, address.Address)
			}
		}
	}
	return addresses, nil
}

func (c *Controller) eventDeleteFunc(obj interface{}) error {
	var nodesIPs []string
	var ports []int32
	var err error

	switch serviceType := obj.(*v1.Service).Spec.Type; serviceType {
	case v1.ServiceTypeNodePort:
		// Check if ExternalIPs is empty
		if len(obj.(*v1.Service).Spec.ExternalIPs) > 0 {
			nodesIPs = obj.(*v1.Service).Spec.ExternalIPs
		} else {
			nodesIPs, err = c.getNodesIPs()
			if err != nil {
				return err
			}
		}
		for _, port := range obj.(*v1.Service).Spec.Ports {
			if port.Protocol == v1.ProtocolTCP {
				ports = append(ports, port.NodePort)
			}
		}

		// Now is time to deregister services from Consul
		for _, nodeAddress := range nodesIPs {
			for _, port := range ports {
				// Add to Consul
				service, err := c.createConsulService(obj.(*v1.Service), nodeAddress, port)
				if err != nil {
					glog.Errorf("Cannot create Consul service: %s", err)
					continue
				}
				// Check if service's already added
				if _, ok := allAddedServices[service.ID]; !ok {
					glog.V(3).Infof("Service %s has already been deleted in Consul", service.ID)
					continue
				}
				consulAgent := c.consulInstance.New(c.cfg, nodeAddress, "")
				err = consulAgent.Deregister(service)
				if err != nil {
					glog.Errorf("Cannot deregister service in Consul: %s", err)
					metrics.ConsulFailure.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
				} else {
					glog.Infof("Service %s has been deregistered in Consul with ID: %s", obj.(*v1.Service).ObjectMeta.Name, service.ID)
					metrics.ConsulSuccess.WithLabelValues("deregister", consulAgent.Config.Address).Inc()
					delete(allAddedServices, service.ID)
				}
			}
		}
	}
	return nil
}

func (c *Controller) createConsulService(svc *v1.Service, address string, port int32) (*consulapi.AgentServiceRegistration, error) {
	service := &consulapi.AgentServiceRegistration{}

	service.ID = fmt.Sprintf("%s-%s-%s-%d", svc.ObjectMeta.Name, svc.ObjectMeta.UID, address, port)
	service.Name = svc.ObjectMeta.Name
	if serviceName, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceNameAnnotation]; ok {
		service.Name = serviceName
	}

	//Add K8sTag from configuration
	service.Tags = []string{c.cfg.Controller.K8sTag}
	service.Tags = append(service.Tags, fmt.Sprintf("uid:%s", svc.ObjectMeta.UID))

	// if set consul.register/service.tags: "tag1,tag2,tag3", then set tags as ["tag1","tag2","tag3"]
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceTags]; ok {
		tags := strings.Split(value, ",")
		for _, tag := range tags {
			service.Tags = append(service.Tags, strings.TrimSpace(tag))
		}
	}
	service.Tags = append(service.Tags, labelsToTags(svc.ObjectMeta.Labels)...)

	service.Port = int(port)
	service.Address = address

	// generate health check for consul
	check := &consulapi.AgentServiceCheck{}
	// !!todo default value from configmap
	check.Interval = "10s"
	check.Timeout = "90s"
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthIntervalAnnotation]; ok {
		check.Interval = fmt.Sprintf("%ss", value)
	}
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthTimeoutAnnotation]; ok {
		check.Timeout = fmt.Sprintf("%ss", value)
	}

	healthHost := address
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthHostAnnotation]; ok {
		healthHost = value
	}

	healthPort := port
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthPortAnnotation]; ok {
		annotationPort, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			glog.Errorf("Can't convert value of %s annotation: %s", ConsulRegisterServiceHealthPortAnnotation, err)
			return nil, err
		} else {
			healthPort = int32(annotationPort)
		}
	}

	healthPath := "/"
	if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthCheckPathAnnotation]; ok {
		healthPath = value
		check.HTTP = fmt.Sprintf("%s://%s:%d%s", "http", healthHost, healthPort, healthPath)
	} else if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthTTLAnnotation]; ok {
		check.TTL = value
	} else if value, ok := svc.ObjectMeta.Annotations[ConsulRegisterServiceHealthTCPAnnotation]; ok && value != "" {
		check.TCP = fmt.Sprintf("%s:%d", healthHost, healthPort)
	} else {
		check.HTTP = fmt.Sprintf("%s://%s:%d%s", "http", healthHost, healthPort, healthPath)
	}

	glog.Infof("%s tags %#v Consul check: %#v", service.Name, service.Tags, check)
	service.Check = check

	return service, nil
}

func labelsToTags(labels map[string]string) []string {
	var tags []string

	for key, value := range labels {
		// if value is equal to "tag" then set only key as tag
		if value == "tag" {
			tags = append(tags, key)
		} else {
			tags = append(tags, fmt.Sprintf("%s:%s", key, value))
		}
	}
	return tags

}

func isRegisterEnabled(obj interface{}) bool {
	if value, ok := obj.(*v1.Service).ObjectMeta.Annotations[ConsulRegisterEnabledAnnotation]; ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			glog.Errorf("Can't convert value of %s annotation: %s", ConsulRegisterEnabledAnnotation, err)
			return false
		}

		if !enabled {
			glog.Infof("Service %s in %s namespace is disabled by annotation. Value: %s", obj.(*v1.Service).ObjectMeta.Name, obj.(*v1.Service).ObjectMeta.Namespace, value)
			return false
		}
	} else {
		glog.V(1).Infof("Service %s in %s namespace will not be registered in Consul. Lack of annotation %s", obj.(*v1.Service).ObjectMeta.Name, obj.(*v1.Service).ObjectMeta.Namespace, ConsulRegisterEnabledAnnotation)
		return false
	}
	return true
}

func (c *Controller) syncPod(ctx context.Context) error {
	panic("unimplemented syncPod")
}
func (c *Controller) watchPod(ctx context.Context) {
	panic("unimplemented watchPod")
}
func (c *Controller) cleanPod(ctx context.Context) error {
	panic("unimplemented cleanPod")
}
