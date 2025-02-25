/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package loadbalancers

import (
	"fmt"
	"net/http"

	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/legacy-cloud-providers/gce"

	compute "google.golang.org/api/compute/v1"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	"k8s.io/klog/v2"
)

// IPAddressType defines if IP address is Managed by controller
type IPAddressType int

const (
	IPAddrUndefined IPAddressType = iota // IP Address type could not be determine due to error is address provisioning.
	IPAddrManaged
	IPAddrUnmanaged
)

// Original file in https://github.com/kubernetes/legacy-cloud-providers/blob/6aa80146c33550e908aed072618bd7f9998837f6/gce/gce_address_manager.go
type addressManager struct {
	logPrefix   string
	svc         gce.CloudAddressService
	name        string
	serviceName string
	targetIP    string
	addressType cloud.LbScheme
	region      string
	subnetURL   string
	tryRelease  bool
	networkTier cloud.NetworkTier
}

func newAddressManager(svc gce.CloudAddressService, serviceName, region, subnetURL, name, targetIP string, addressType cloud.LbScheme, networkTier cloud.NetworkTier) *addressManager {
	return &addressManager{
		svc:         svc,
		logPrefix:   fmt.Sprintf("AddressManager(%q)", name),
		region:      region,
		serviceName: serviceName,
		name:        name,
		targetIP:    targetIP,
		addressType: addressType,
		tryRelease:  true,
		subnetURL:   subnetURL,
		networkTier: networkTier,
	}
}

// HoldAddress will ensure that the IP is reserved with an address - either owned by the controller
// or by a user. If the address is not the addressManager.name, then it's assumed to be a user's address.
// The string returned is the reserved IP address and IPAddressType indicating if IP address is managed by controller.
func (am *addressManager) HoldAddress() (string, IPAddressType, error) {
	// HoldAddress starts with retrieving the address that we use for this load balancer (by name).
	// Retrieving an address by IP will indicate if the IP is reserved and if reserved by the user
	// or the controller, but won't tell us the current state of the controller's IP. The address
	// could be reserving another address; therefore, it would need to be deleted. In the normal
	// case of using a controller address, retrieving the address by name results in the fewest API
	// calls since it indicates whether a Delete is necessary before Reserve.
	klog.V(4).Infof("%v: attempting hold of IP %q Type %q", am.logPrefix, am.targetIP, am.addressType)
	// Get the address in case it was orphaned earlier
	addr, err := am.svc.GetRegionAddress(am.name, am.region)
	if err != nil && !utils.IsNotFoundError(err) {
		return "", IPAddrUndefined, err
	}

	if addr != nil {
		// If address exists, check if the address had the expected attributes.
		validationError := am.validateAddress(addr)
		if validationError == nil {
			klog.V(4).Infof("%v: address %q already reserves IP %q Type %q. No further action required.", am.logPrefix, addr.Name, addr.Address, addr.AddressType)
			return addr.Address, IPAddrManaged, nil
		}

		klog.V(2).Infof("%v: deleting existing address because %v", am.logPrefix, validationError)
		err := am.svc.DeleteRegionAddress(addr.Name, am.region)
		if err != nil {
			if utils.IsNotFoundError(err) {
				klog.V(4).Infof("%v: address %q was not found. Ignoring.", am.logPrefix, addr.Name)
			} else {
				return "", IPAddrUndefined, err
			}
		} else {
			klog.V(4).Infof("%v: successfully deleted previous address %q", am.logPrefix, addr.Name)
		}
	}

	return am.ensureAddressReservation()
}

// ReleaseAddress will release the address if it's owned by the controller.
func (am *addressManager) ReleaseAddress() error {
	if !am.tryRelease {
		klog.V(4).Infof("%v: not attempting release of address %q.", am.logPrefix, am.targetIP)
		return nil
	}

	klog.V(4).Infof("%v: releasing address %q named %q", am.logPrefix, am.targetIP, am.name)
	// Controller only ever tries to unreserve the address named with the load balancer's name.
	err := am.svc.DeleteRegionAddress(am.name, am.region)
	if err != nil {
		if utils.IsNotFoundError(err) {
			klog.Warningf("%v: address %q was not found. Ignoring.", am.logPrefix, am.name)
			return nil
		}

		return err
	}

	klog.V(4).Infof("%v: successfully released IP %q named %q", am.logPrefix, am.targetIP, am.name)
	return nil
}

// ensureAddressReservation reserves ip address and returns address as a string,
// IPAddressType indicating whether ip address is managed by controller and error.
func (am *addressManager) ensureAddressReservation() (string, IPAddressType, error) {
	// Try reserving the IP with controller-owned address name
	// If am.targetIP is an empty string, a new IP will be created.
	newAddr := &compute.Address{
		Name:        am.name,
		Description: fmt.Sprintf(`{"kubernetes.io/service-name":"%s"}`, am.serviceName),
		Address:     am.targetIP,
		AddressType: string(am.addressType),
		Subnetwork:  am.subnetURL,
	}
	// NetworkTier is supported only for External IP Address
	if am.addressType == cloud.SchemeExternal {
		newAddr.NetworkTier = am.networkTier.ToGCEValue()
	}

	reserveErr := am.svc.ReserveRegionAddress(newAddr, am.region)
	if reserveErr == nil {
		if newAddr.Address != "" {
			klog.V(4).Infof("%v: successfully reserved IP %q with name %q", am.logPrefix, newAddr.Address, newAddr.Name)
			return newAddr.Address, IPAddrManaged, nil
		}

		// If an ip address was not specified, get the newly created address resource to determine the assigned address.
		addr, err := am.svc.GetRegionAddress(newAddr.Name, am.region)
		if err != nil {
			return "", IPAddrUndefined, err
		}

		klog.V(4).Infof("%v: successfully created address %q which reserved IP %q", am.logPrefix, addr.Name, addr.Address)
		return addr.Address, IPAddrManaged, nil
	}

	if utils.IsNetworkTierMismatchGCEError(reserveErr) {
		receivedNetworkTier := cloud.NetworkTierPremium
		if receivedNetworkTier == am.networkTier {
			// We don't have information of current ephemeral IP address Network Tier since
			// we try to reserve the address so we need to check against desired Network Tier and set the opposite one.
			// This is just for error message.
			receivedNetworkTier = cloud.NetworkTierStandard
		}
		resource := fmt.Sprintf("Reserved static IP (%v)", am.name)
		networkTierError := utils.NewNetworkTierErr(resource, string(am.networkTier), string(receivedNetworkTier))
		return "", IPAddrUndefined, networkTierError
	}

	if !utils.IsHTTPErrorCode(reserveErr, http.StatusConflict) && !utils.IsHTTPErrorCode(reserveErr, http.StatusBadRequest) {
		// If the IP is already reserved:
		//    by an internal address: a StatusConflict is returned
		//    by an external address: a BadRequest is returned
		return "", IPAddrUndefined, reserveErr
	}

	// If the target IP was empty, we cannot try to find which IP caused a conflict.
	// If the name was already used, then the next sync will attempt deletion of that address.
	if am.targetIP == "" {
		return "", IPAddrUndefined, fmt.Errorf("failed to reserve address %q with no specific IP, err: %v", am.name, reserveErr)
	}

	// Reserving the address failed due to a conflict or bad request. The address manager just checked that no address
	// exists with the name, so it may belong to the user.
	addr, err := am.svc.GetRegionAddressByIP(am.region, am.targetIP)
	if err != nil {
		return "", IPAddrUndefined, fmt.Errorf("failed to get address by IP %q after reservation attempt, err: %q, reservation err: %q", am.targetIP, err, reserveErr)
	}

	// Check that the address attributes are as required.
	if err := am.validateAddress(addr); err != nil {
		return "", IPAddrUndefined, fmt.Errorf("address (%q) validation failed, err: %w", addr.Name, err)
	}

	if am.isManagedAddress(addr) {
		// The address with this name is checked at the beginning of 'HoldAddress()', but for some reason
		// it was re-created by this point. May be possible that two controllers are running.
		klog.Warningf("%v: address %q unexpectedly existed with IP %q.", am.logPrefix, addr.Name, am.targetIP)
		return addr.Address, IPAddrManaged, nil
	}
	// If the retrieved address is not named with the loadbalancer name, then the controller does not own it, but will allow use of it.
	klog.V(4).Infof("%v: address %q was already reserved with name: %q, description: %q", am.logPrefix, am.targetIP, addr.Name, addr.Description)
	am.tryRelease = false
	return addr.Address, IPAddrUnmanaged, nil

}

func (am *addressManager) validateAddress(addr *compute.Address) error {
	if am.targetIP != "" && am.targetIP != addr.Address {
		return fmt.Errorf("IP mismatch, expected %q, actual: %q", am.targetIP, addr.Address)
	}
	if addr.AddressType != string(am.addressType) {
		return fmt.Errorf("address type mismatch, expected %q, actual: %q", am.addressType, addr.AddressType)
	}
	if addr.NetworkTier != am.networkTier.ToGCEValue() {
		return utils.NewNetworkTierErr(fmt.Sprintf("Static IP (%v)", am.name), am.networkTier.ToGCEValue(), addr.NetworkTier)
	}
	return nil
}

func (am *addressManager) isManagedAddress(addr *compute.Address) bool {
	return addr.Name == am.name
}

func ensureAddressDeleted(svc gce.CloudAddressService, name, region string) error {
	return utils.IgnoreHTTPNotFound(svc.DeleteRegionAddress(name, region))
}

// TearDownAddressIPIfNetworkTierMismatch this function tear down controller managed address IP if it has a wrong Network Tier
func (am *addressManager) TearDownAddressIPIfNetworkTierMismatch() error {
	if am.targetIP == "" {
		return nil
	}
	addr, err := am.svc.GetRegionAddressByIP(am.region, am.targetIP)
	if utils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if addr != nil && addr.NetworkTier != am.networkTier.ToGCEValue() {
		if !am.isManagedAddress(addr) {
			return utils.NewNetworkTierErr(fmt.Sprintf("User specific address IP (%v)", am.name), string(am.networkTier), addr.NetworkTier)
		}
		klog.V(3).Infof("Deleting IP address %v because has wrong network tier", am.targetIP)
		if err := am.svc.DeleteRegionAddress(addr.Name, am.targetIP); err != nil {
			klog.Errorf("Unable to delete region address %s on target ip %s, err: %v", addr.Name, am.targetIP, err)
		}
	}
	return nil
}
