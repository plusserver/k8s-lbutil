// Shared constants and functions for controllers that use k8s-ipam to configure loadbalancers.

package lbutil

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"

	ipamv1 "github.com/Nexinto/k8s-ipam/pkg/apis/ipam.nexinto.com/v1"
	ipamclientset "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned"
	ipamlisterv1 "github.com/Nexinto/k8s-ipam/pkg/client/listers/ipam.nexinto.com/v1"
)

const (

	// If set to any value, a VIP will be configured.
	AnnNxReqVIP = "nexinto.com/req-vip"

	// This will be set to the VIP. If set, the Loadbalancer was configured successfully.
	AnnNxVIP = "nexinto.com/vip"

	// This will be the VIP chosen for the service.
	AnnNxAssignedVIP = "nexinto.com/assigned-vip"

	// Set this to explicitly choose a VIP provider.
	AnnNxVIPProvider = "nexinto.com/vip-provider"

	// The active provider for this VIP.
	AnnNxVIPActiveProvider = "nexinto.com/vip-active-provider"
)

// Create an event for an object.
func MakeEvent(kube kubernetes.Interface, o metav1.Object, message string, warn bool) error {
	var t string
	if warn {
		t = "Warning"
	} else {
		t = "Normal"
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: o.GetName(),
		},
		InvolvedObject: corev1.ObjectReference{
			Name:            o.GetName(),
			Namespace:       o.GetNamespace(),
			APIVersion:      "v1",
			UID:             o.GetUID(),
			Kind:            "IpAddress",
			ResourceVersion: o.GetResourceVersion(),
		},
		Message:        message,
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Type:           t,
	}

	_, err := kube.CoreV1().Events(o.GetNamespace()).Create(event)
	return err
}

// Create a Warning Event for the object and also return it as an error.
func LogEventAndFail(kube kubernetes.Interface, o metav1.Object, message string) error {
	log.Error(message)
	_ = MakeEvent(kube, o, message, true)
	return fmt.Errorf(message)
}

// Checks if the service is annotated with a valid VIP, if not, work towards that. Return value ok is true if the service / its
// annotation is valid and the caller can continue and use the VIP;
// if it is false, the caller must stop processing and retry later.
// If there is no error and it is ok to continue, use the returned "newservice" to query the VIP or to make changes to the Service,
// not your original service because the original came from the cache and should not be modified.
// If 'needsUpdate' is true, then the service copy was modified and needs to be updated by the caller.
func EnsureVIP(kube kubernetes.Interface, ipamclient ipamclientset.Interface, addressLister ipamlisterv1.IpAddressLister,
	service *corev1.Service, controllerName string, requireAnnotation bool) (ok bool, needsUpdate bool, newservice *corev1.Service, err error) {

	if service.Spec.Type != corev1.ServiceTypeNodePort {
		log.Debugf("skipping '%s-%s': not a NodePort", service.Namespace, service.Name)
		return false, false, nil, nil
	}

	if requireAnnotation && service.Annotations[AnnNxReqVIP] == "" {
		log.Debugf("skipping '%s-%s': REQUIRE_TAG is true and service does not have our annotation", service.Namespace, service.Name)
		return false, false, nil, nil
	}

	if service.Annotations[AnnNxVIPProvider] != "" && service.Annotations[AnnNxVIPProvider] != controllerName {
		log.Debugf("skipping '%s-%s': service requests provider '%s'", service.Namespace, service.Name, service.Annotations[AnnNxVIPProvider])
		return false, false, nil, nil
	}

	if service.Annotations[AnnNxVIPActiveProvider] != "" && service.Annotations[AnnNxVIPActiveProvider] != controllerName {
		log.Debugf("skipping '%s-%s': service is managed by provider '%s'", service.Namespace, service.Name, service.Annotations[AnnNxVIPActiveProvider])
		return false, false, nil, nil
	}

	if service.Annotations[AnnNxVIPActiveProvider] == "" {

		log.Debugf("trying to claim the service")

		// Try to claim the service
		newservice := service.DeepCopy()
		if newservice.Annotations == nil {
			newservice.Annotations = map[string]string{}
		}
		newservice.Annotations[AnnNxVIPActiveProvider] = controllerName

		return false, true, newservice, nil
	}

	addr, addrLookupErr := addressLister.IpAddresses(service.Namespace).Get(service.Name)
	if err != nil && !errors.IsNotFound(addrLookupErr) {
		// General error getting the address. NotFound is handled below depending on context.
		return false, false, nil, fmt.Errorf("error looking up ipaddress object for service '%s-%s': %s", service.Namespace, service.Name, err.Error())
	}

	if service.Annotations[AnnNxAssignedVIP] == "" {
		// A VIP is not yet set for the Service.

		if errors.IsNotFound(addrLookupErr) {
			log.Debugf("no address for '%s-%s' exists", service.Namespace, service.Name)
			return false, false, nil, RequestAddress(kube, ipamclient, service)
		}

		if addr.Status.Address == "" {
			log.Debugf("ip address '%s-%s' has no address yet", addr.Namespace, addr.Name)
			return false, false, nil, nil
		}

		newservice := StoreVIP(addr.Status.Address, kube, service)

		return true, true, newservice, nil
	}

	if errors.IsNotFound(addrLookupErr) {
		// The IP address object for our service has somehow disappeared. Reset the stored address
		// and restart the process.
		log.Infof("assigned IP address for service '%s-%s' has disappeared (was %s)", service.Namespace, service.Name, service.Annotations[AnnNxAssignedVIP])
		newservice := StoreVIP("", kube, service)
		return false, true, newservice, err
	}

	if addr.Status.Address != service.Annotations[AnnNxAssignedVIP] {
		// The IP address has changed. Set the new address and continue.
		log.Infof("assigned IP address for service '%s-%s' has changed (from %s to %s)", service.Namespace, service.Name, addr.Status.Address, service.Annotations[AnnNxAssignedVIP])
		newservice := StoreVIP("", kube, service)
		return true, true, newservice, err
	}

	return true, needsUpdate, service, nil
}

// Create a new IpAddress Object for a Service.
func RequestAddress(kube kubernetes.Interface, ipamclient ipamclientset.Interface, service *corev1.Service) error {
	addr := ipamv1.IpAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				Name:       service.GetName(),
				Kind:       "Service",
				APIVersion: "v1",
				UID:        service.GetUID(),
			}},
		},
		Spec: ipamv1.IpAddressSpec{
			Description: fmt.Sprintf("created for service %s", service.Name),
		},
	}

	_, err := ipamclient.IpamV1().IpAddresses(service.Namespace).Create(&addr)
	if err != nil {
		return fmt.Errorf("failed to create ip address request for service '%s-%s': %s", service.Namespace, service.Name, err.Error())
	}

	log.Infof("created ip address request for '%s-%s'", service.Namespace, service.Name)

	return nil
}

func StoreVIP(vip string, kube kubernetes.Interface, service *corev1.Service) *corev1.Service {
	o2 := service.DeepCopy()
	o2.Annotations[AnnNxAssignedVIP] = vip

	log.Debugf("storing assigned VIP '%s' for service '%s-%s'", vip, service.Namespace, service.Name)
	_ = MakeEvent(kube, service, fmt.Sprintf("assigned VIP %s", vip), false)

	return o2
}

// If an IP address object changes and a Service is an owner, wake up that Service.
func IpAddressCreatedOrUpdated(serviceQueue workqueue.RateLimitingInterface, address *ipamv1.IpAddress) {
	if address.Status.Address != "" {
		for _, ref := range address.OwnerReferences {
			if ref.Kind == "Service" && ref.APIVersion == "v1" {
				log.Debugf("change to ipaddress '%s-%s' wakes up service '%s-%s'", address.Namespace, address.Name, address.Namespace, ref.Name)
				serviceQueue.Add(fmt.Sprintf("%s/%s", address.Namespace, ref.Name))
			}
		}
	}
}

// If an IP address is deleted and a Service is the owner and it still exists, remove
// the VIP annotation and wake up the service so the service can retry requesting loadbalancing.
func IpAddressDeleted(kubernetes kubernetes.Interface, serviceLister corelisterv1.ServiceLister, address *ipamv1.IpAddress) error {
	for _, ref := range address.OwnerReferences {
		if ref.Kind == "Service" && ref.APIVersion == "v1" {
			service, err := serviceLister.Services(metav1.NamespaceAll).Get(ref.Name)
			if err != nil {
				if errors.IsNotFound(err) {
					continue
				} else {
					return err
				}
			}
			if service.Annotations[AnnNxAssignedVIP] != "" {
				log.Debugf("ipaddress '%s-%s' was deleted; resetting service '%s-%s'", address.Namespace, address.Name, address.Namespace, service.Name)
				newService := service.DeepCopy()
				newService.Annotations[AnnNxAssignedVIP] = ""
				_, err = kubernetes.CoreV1().Services(newService.Namespace).Update(newService)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Simulates the behaviour of the ipam controller.
func SimIPAM(ipamclient ipamclientset.Interface) error {
	i := 1

	addrs, err := ipamclient.IpamV1().IpAddresses(metav1.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, addr := range addrs.Items {
		if addr.Status.Address == "" {
			addr.Status.Address = fmt.Sprintf("10.0.0.%d", i)
			i++
			_, err := ipamclient.IpamV1().IpAddresses(addr.Namespace).Update(&addr)

			log.Debugf("[simIPAM] assign: %s/%s -> %s", addr.Namespace, addr.Name, addr.Status.Address)

			if err != nil {
				return err
			}
		}
	}

	return nil
}
