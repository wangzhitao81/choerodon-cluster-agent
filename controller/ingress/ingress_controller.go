package ingress

import (
	"encoding/json"
	"fmt"
	"github.com/choerodon/choerodon-cluster-agent/manager"
	"github.com/choerodon/choerodon-cluster-agent/pkg/model/kubernetes"
	"k8s.io/apimachinery/pkg/labels"
	"time"

	"github.com/golang/glog"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appv1 "k8s.io/client-go/informers/extensions/v1beta1"
	appv1_lister "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/choerodon/choerodon-cluster-agent/pkg/model"
)

var (
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

type controller struct {
	queue workqueue.RateLimitingInterface
	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration
	lister           appv1_lister.IngressLister
	responseChan     chan<- *model.Packet
	ingresssSynced   cache.InformerSynced
	namespaces       *manager.Namespaces
}

func (c *controller) resourceSync()  {
	namespaces := c.namespaces.GetAll()
	for  _,ns := range namespaces {
		pods, err := c.lister.Ingresses(ns).List(labels.NewSelector())
		if err != nil {
			glog.Fatal("can not list resource, no rabc bind, exit !")
		} else {
			var serviceList []string
			for _, pod := range pods {
				if pod.Labels[model.ReleaseLabel] != "" {
					serviceList = append(serviceList, pod.GetName())
				}
			}
			resourceList := &kubernetes.ResourceList{
				Resources:    serviceList,
				ResourceType: "Ingress",
			}
			content, err := json.Marshal(resourceList)
			if err != nil {
				glog.Fatal("marshal ingress list error")
			} else {
				response := &model.Packet{
					Key:     fmt.Sprintf("env:%s", ns),
					Type:    model.ResourceSync,
					Payload: string(content),
				}
				c.responseChan <- response
			}
		}
	}
}

func NewIngressController(ingressInformer appv1.IngressInformer, responseChan chan<- *model.Packet, namespaces  *manager.Namespaces) *controller {

	c := &controller{
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ingress"),
		workerLoopPeriod: time.Second,
		lister:           ingressInformer.Lister(),
		responseChan:     responseChan,
		namespaces:		namespaces,
	}

	ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueIngress,
		UpdateFunc: func(old, new interface{}) {
			newIngress := new.(*extensions.Ingress)
			oldIngress := old.(*extensions.Ingress)
			if newIngress.ResourceVersion == oldIngress.ResourceVersion {
				return
			}
			c.enqueueIngress(new)
		},
		DeleteFunc: c.enqueueIngress,
	})
	c.ingresssSynced = ingressInformer.Informer().HasSynced
	return c
}

func (c *controller) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.ingresssSynced); !ok {
		glog.Fatal("failed to wait for caches to sync")
	}

	//resources, err := c.lister.Ingresses(c.namespace).List(labels.NewSelector())
	//if err != nil {
	//	glog.Fatal("failed list ingress")
	//} else {
	//	var resourceList []string
	//	for _, resource := range resources {
	//		if resource.Labels[model.NetworkLabel] != "" {
	//			resourceList = append(resourceList, resource.GetName())
	//		}
	//	}
	//	resourceListResp := &kubernetes.ResourceList{
	//		Resources:    resourceList,
	//		ResourceType: "Ingress",
	//	}
	//	content, err := json.Marshal(resourceListResp)
	//	if err != nil {
	//		glog.Fatal("marshal ingress list error")
	//	} else {
	//		response := &model.Packet{
	//			Key:     fmt.Sprintf("env:%s", c.namespace),
	//			Type:    model.ResourceSync,
	//			Payload: string(content),
	//		}
	//		c.responseChan <- response
	//	}
	//}
	c.resourceSync()

	// Launch two workers to process Foo resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	glog.V(1).Info("Shutting down ingress workers")
}
func (c *controller) enqueueIngress(obj interface{}) {
	var key string
	var err error
	if key, err = keyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.queue.AddRateLimited(key)
}

func (c *controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *controller) processNextWorkItem() bool {
	key, shutdown := c.queue.Get()

	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	forget, err := c.syncHandler(key.(string))
	if err == nil {
		if forget {
			c.queue.Forget(key)
		}
		return true
	}

	runtime.HandleError(fmt.Errorf("error syncing '%s': %s", key, err.Error()))
	c.queue.AddRateLimited(key)

	return true
}

func (c *controller) syncHandler(key string) (bool, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return true, nil
	}
	if !c.namespaces.Contain(namespace) {
		return true, nil
	}

	ingress, err := c.lister.Ingresses(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			c.responseChan <- newIngressDelRep(name, namespace)
			glog.Warningf("ingress '%s' in work queue no longer exists", key)
			return true, nil
		}
		return false, err
	}

	if ingress.Labels[model.NetworkLabel] != "" {
		glog.V(2).Info(ingress.Labels[model.NetworkLabel], ":", ingress)
		c.responseChan <- newIngressRep(ingress)
	}
	return true, nil
}

func newIngressDelRep(name string, namespace string) *model.Packet {

	return &model.Packet{
		Key:  fmt.Sprintf("env:%s.Ingress:%s", namespace, name),
		Type: model.ResourceDelete,
	}
}

func newIngressRep(ingress *extensions.Ingress) *model.Packet {
	payload, err := json.Marshal(ingress)
	if err != nil {
		glog.Error(err)
	}
	return &model.Packet{
		Key:     fmt.Sprintf("env:%s.Ingress:%s", ingress.Namespace, ingress.Name),
		Type:    model.ResourceUpdate,
		Payload: string(payload),
	}
}
