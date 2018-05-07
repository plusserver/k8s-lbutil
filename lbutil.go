// Shared constants and functions for controllers that use k8s-ipam to configure loadbalancers.

package lbutil

import (
	"fmt"
	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1 "github.com/Nexinto/k8s-ipam/pkg/apis/ipam.nexinto.com/v1"
	ipamclientset "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned"
	ipamlisterv1 "github.com/Nexinto/k8s-ipam/pkg/client/listers/ipam.nexinto.com/v1"
)

const (

	// If set to any value, a VIP will be configured.
	AnnNxReqVIP = "nexinto.com/req-vip"

	// This will be set to the VIP.
	AnnNxVIP = "nexinto.com/vip"

	// This annotation contains the VIP that is cofigured by the k8s-bigip-ctlr.
	AnnVirtualServerIP = "virtual-server.f5.com/ip"

	// The k8s-bigip-ctlr will set this to the VIP once it is configured on the loadbalancer.
	AnnVirtualServerIPStatus = "status.virtual-server.f5.com/ip"

	// This annotation selects one or more SSL profile
	AnnNxSSLProfiles = "nexinto.com/vip-ssl-profiles"

	// VIP Mode (http or tcp, http is the default)
	AnnNxVipMode = "nexinto.com/req-vip-mode"

	// Set this to explicitly choose a VIP provider.
	AnnNxVIPProvider = "nexinto.com/vip-provider"

	// bigip provider
	AnnNxVIPProviderBigIP = "bigip"

	// fortigate provider
	AnnNxVIPProviderFortigate = "fortigate"

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
func EnsureVIP(kube kubernetes.Interface, ipamclient ipamclientset.Interface, addressLister ipamlisterv1.IpAddressLister,
	service *corev1.Service, controllerName string, requireAnnotation bool) (ok bool, newservice *corev1.Service, err error) {

	if service.Spec.Type != corev1.ServiceTypeNodePort {
		log.Debugf("skipping '%s-%s': not a NodePort", service.Namespace, service.Name)
		return false, nil, nil
	}

	if requireAnnotation && service.Annotations[AnnNxReqVIP] == "" {
		log.Debugf("skipping '%s-%s': REQUIRE_TAG is true and service does not have our annotation", service.Namespace, service.Name)
		return false, nil, nil
	}

	if service.Annotations[AnnNxVIPProvider] != "" && service.Annotations[AnnNxVIPProvider] != controllerName {
		log.Debugf("skipping '%s-%s': service requests provider '%s'", service.Namespace, service.Name, service.Annotations[AnnNxVIPProvider])
		return false, nil, nil
	}

	if service.Annotations[AnnNxVIPActiveProvider] != "" && service.Annotations[AnnNxVIPActiveProvider] != controllerName {
		log.Debugf("skipping '%s-%s': service is managed by provider '%s'", service.Namespace, service.Name, service.Annotations[AnnNxVIPActiveProvider])
		return false, nil, nil
	}

	if service.Annotations[AnnNxVIPActiveProvider] == "" {
		// Try to claim the service
		newservice := service.DeepCopy()
		newservice.Annotations[AnnNxVIPActiveProvider] = controllerName

		_, err := kube.CoreV1().Services(service.Namespace).Update(newservice)
		if err != nil {
			return false, nil, fmt.Errorf("error updating service '%s-%s': %s", service.Namespace, service.Name, err.Error())
		}

		return false, nil, nil
	}

	addr, addrLookupErr := addressLister.IpAddresses(service.Namespace).Get(service.Name)
	if err != nil && !errors.IsNotFound(addrLookupErr) {
		// General error getting the address. NotFound is handled below depending on context.
		return false, nil, fmt.Errorf("error looking up ipaddress object for service '%s-%s': %s", service.Namespace, service.Name, err.Error())
	}

	if service.Annotations[AnnNxVIP] == "" {
		// A VIP is not yet set for the Service.

		if errors.IsNotFound(addrLookupErr) {
			log.Debugf("no address for '%s-%s' exists", service.Namespace, service.Name)
			return false, nil, RequestAddress(kube, ipamclient, service)
		}

		if addr.Status.Address == "" {
			log.Debugf("ip address '%s-%s' has no address yet", addr.Namespace, addr.Name)
			return false, nil, nil
		}

		newservice, err := StoreVIP(addr.Status.Address, kube, service)
		if err != nil {
			return false, nil, err
		}

		return true, newservice, nil
	}

	if errors.IsNotFound(addrLookupErr) {
		// The IP address object for our service has somehow disappeared. Reset the stored address
		// and restart the process.
		log.Infof("assigned IP address for service '%s-%s' has disappeared (was %s)", service.Namespace, service.Name, service.Annotations[AnnNxVIP])
		newservice, err := StoreVIP("", kube, service)
		return false, newservice, err
	}

	if addr.Status.Address != service.Annotations[AnnNxVIP] {
		// The IP address has changed. Set the new address and continue.
		log.Infof("assigned IP address for service '%s-%s' has changed (from %s to %s)", service.Namespace, service.Name, addr.Status.Address, service.Annotations[AnnNxVIP])
		newservice, err := StoreVIP("", kube, service)
		return true, newservice, err
	}

	return true, service, nil
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

func StoreVIP(vip string, kube kubernetes.Interface, service *corev1.Service) (*corev1.Service, error) {
	o2 := service.DeepCopy()
	o2.Annotations[AnnNxVIP] = vip

	_, err := kube.CoreV1().Services(service.Namespace).Update(o2)
	if err != nil {
		return nil, fmt.Errorf("error updating service '%s-%s': %s", service.Namespace, service.Name, err.Error())
	}

	_ = MakeEvent(kube, service, fmt.Sprintf("assigned VIP %s", vip), false)

	return o2, nil
}
