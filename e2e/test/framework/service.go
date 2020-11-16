package framework

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

func (i *lbInvocation) createOrUpdateService(selector, annotations map[string]string, ports []core.ServicePort, isSessionAffinityClientIP bool, isCreate bool) error {
	var sessionAffinity core.ServiceAffinity = "None"
	if isSessionAffinityClientIP {
		sessionAffinity = "ClientIP"
	}
	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        TestServerResourceName,
			Namespace:   i.Namespace(),
			Annotations: annotations,
			Labels: map[string]string{
				"app": "test-server-" + i.app,
			},
		},
		Spec: core.ServiceSpec{
			Ports:           ports,
			Selector:        selector,
			Type:            core.ServiceTypeLoadBalancer,
			SessionAffinity: sessionAffinity,
		},
	}

	service := i.kubeClient.CoreV1().Services(i.Namespace())
	if isCreate {
		_, err := service.Create(svc)
		if err != nil {
			return err
		}
	} else {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			options := metav1.GetOptions{}
			resource, err := service.Get(TestServerResourceName, options)
			if err != nil {
				return err
			}
			svc.ObjectMeta.ResourceVersion = resource.ResourceVersion
			svc.Spec.ClusterIP = resource.Spec.ClusterIP
			_, err = service.Update(svc)
			return err
		}); err != nil {
			return err
		}
	}
	return i.waitForServerReady()
}

func (i *lbInvocation) CreateService(selector, annotations map[string]string, ports []core.ServicePort, isSessionAffinityClientIP bool) error {
	err := i.createOrUpdateService(selector, annotations, ports, isSessionAffinityClientIP, true)
	if err != nil {
		return err
	}
	return i.waitForEnsured()
}
func (i *lbInvocation) UpdateService(selector, annotations map[string]string, ports []core.ServicePort, isSessionAffinityClientIP bool) error {
	err := i.deleteEvents()
	if err != nil {
		return err
	}
	err = i.createOrUpdateService(selector, annotations, ports, isSessionAffinityClientIP, false)
	if err != nil {
		return err
	}
	return i.waitForEnsured()
}

func (i *lbInvocation) DeleteService() error {
	err := i.kubeClient.CoreV1().Services(i.Namespace()).Delete(TestServerResourceName, nil)
	return err
}

func (i *lbInvocation) waitForServerReady() error {
	var err error
	var ep *core.Endpoints
	for it := 0; it < MaxRetry; it++ {
		ep, err = i.kubeClient.CoreV1().Endpoints(i.Namespace()).Get(TestServerResourceName, metav1.GetOptions{})
		if err == nil {
			if len(ep.Subsets) > 0 {
				if len(ep.Subsets[0].Addresses) > 0 {
					break
				}
			}
		}
		glog.Infoln("Waiting for TestServer to be ready")
		time.Sleep(time.Second * 5)
	}
	return err
}

func (i *lbInvocation) deleteEvents() error {
	return i.kubeClient.CoreV1().Events(i.Namespace()).DeleteCollection(&metav1.DeleteOptions{}, metav1.ListOptions{FieldSelector: "involvedObject.kind=Service"})
}

func (i *lbInvocation) waitForEnsured() error {
	var timeoutSeconds int64 = 30
	watcher, err := i.kubeClient.CoreV1().Events(i.Namespace()).Watch(metav1.ListOptions{
		FieldSelector:  "involvedObject.kind=Service",
		Watch:          true,
		TimeoutSeconds: &timeoutSeconds})
	if err != nil {
		return err
	}

	ch := watcher.ResultChan()

	for event := range ch {
		event, ok := event.Object.(*core.Event)
		if !ok {
			log.Fatal("unexpected type")
			return errors.Errorf("failed to poll event")
		}
		switch event.Reason {
		case "CreatingLoadBalancerFailed":
			s, err := json.MarshalIndent(event, "", "\t")
			if err != nil {
				return err
			}
			return errors.Errorf("Received failure: %s", string(s))
		case "EnsuredLoadBalancer":
			return nil
		}
	}
	return nil
}

func (i *lbInvocation) getLoadBalancerURLs() ([]string, error) {
	var serverAddr []string

	svc, err := i.GetServiceWithLoadBalancerStatus(TestServerResourceName, i.Namespace())
	if err != nil {
		return serverAddr, err
	}

	ips := make([]string, 0)
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		ips = append(ips, ingress.IP)
	}

	var ports []int32
	if len(svc.Spec.Ports) > 0 {
		for _, port := range svc.Spec.Ports {
			if port.NodePort > 0 {
				ports = append(ports, port.Port)
			}
		}
	}

	for _, port := range ports {
		for _, ip := range ips {
			u, err := url.Parse(fmt.Sprintf("http://%s:%d", ip, port))
			if err != nil {
				return nil, err
			}
			serverAddr = append(serverAddr, u.String())
		}
	}

	return serverAddr, nil
}

func (i *lbInvocation) GetServiceWithLoadBalancerStatus(name, namespace string) (*core.Service, error) {
	var (
		svc *core.Service
		err error
	)
	err = wait.PollImmediate(2*time.Second, 20*time.Minute, func() (bool, error) {
		svc, err = i.kubeClient.CoreV1().Services(namespace).Get(name, metav1.GetOptions{})
		if err != nil || len(svc.Status.LoadBalancer.Ingress) == 0 { // retry
			return false, nil
		} else {
			return true, nil
		}
	})
	if err != nil {
		return nil, errors.Errorf("failed to get Status.LoadBalancer.Ingress for service %s/%s", name, namespace)
	}
	return svc, nil
}

func (i *lbInvocation) testServerServicePorts() []core.ServicePort {
	return []core.ServicePort{
		{
			Name:       "http-1",
			Port:       80,
			TargetPort: intstr.FromInt(8080),
			Protocol:   "TCP",
		},
	}
}
