package proxy

import (
	"fmt"
	"hash/fnv"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cloudnativelabs/kube-router/pkg/utils"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"
)

const (
	interfaceWaitSleepTime = 100 * time.Millisecond
)

func attemptNamespaceResetAfterError(hostNSHandle netns.NsHandle) {
	err := netns.Set(hostNSHandle)
	if err != nil {
		klog.Errorf("failed to set hostNetworkNamespace while resetting namespace after a previous error due to %v",
			err)
	}
	activeNetworkNamespaceHandle, err := netns.Get()
	if err != nil {
		klog.Errorf("failed to confirm activeNetworkNamespace while resetting namespace after "+
			"a previous error due to %v", err)
		return
	}
	klog.V(2).Infof("Current network namespace after revert namespace to host network namespace: %s",
		activeNetworkNamespaceHandle.String())
	_ = activeNetworkNamespaceHandle.Close()
}

func (ln *linuxNetworking) configureContainerForDSR(
	vip, endpointIP, containerID string, pid int, hostNetworkNamespaceHandle netns.NsHandle) error {
	endpointNamespaceHandle, err := netns.GetFromPid(pid)
	if err != nil {
		return fmt.Errorf("failed to get endpoint namespace (containerID=%s, pid=%d, error=%v)",
			containerID, pid, err)
	}
	defer utils.CloseCloserDisregardError(&endpointNamespaceHandle)

	// LINUX NAMESPACE SHIFT - It is important to note that from here until the end of the function (or until an error)
	// all subsequent commands are executed from within the container's network namespace and NOT the host's namespace.
	err = netns.Set(endpointNamespaceHandle)
	if err != nil {
		return fmt.Errorf("failed to enter endpoint namespace (containerID=%s, pid=%d, error=%v)",
			containerID, pid, err)
	}

	activeNetworkNamespaceHandle, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get activeNetworkNamespace due to %v", err)
	}
	klog.V(2).Infof("Current network namespace after netns. Set to container network namespace: %s",
		activeNetworkNamespaceHandle.String())
	_ = activeNetworkNamespaceHandle.Close()

	// TODO: fix boilerplate `netns.Set(hostNetworkNamespaceHandle)` code. Need a robust
	// way to switch back to old namespace, pretty much all things will go wrong if we dont switch back

	// create an ipip tunnel interface inside the endpoint container
	tunIf, err := netlink.LinkByName(KubeTunnelIf)
	if err != nil {
		if err.Error() != IfaceNotFound {
			attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
			return fmt.Errorf("failed to verify if ipip tunnel interface exists in endpoint %s namespace due "+
				"to %v", endpointIP, err)
		}

		klog.V(2).Infof("Could not find tunnel interface %s in endpoint %s so creating one.",
			KubeTunnelIf, endpointIP)
		ipTunLink := netlink.Iptun{
			LinkAttrs: netlink.LinkAttrs{Name: KubeTunnelIf},
			Local:     net.ParseIP(endpointIP),
		}
		err = netlink.LinkAdd(&ipTunLink)
		if err != nil {
			attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
			return fmt.Errorf("failed to add ipip tunnel interface in endpoint namespace due to %v", err)
		}

		// this is ugly, but ran into issue multiple times where interface did not come up quickly.
		for retry := 0; retry < 60; retry++ {
			time.Sleep(interfaceWaitSleepTime)
			tunIf, err = netlink.LinkByName(KubeTunnelIf)
			if err == nil {
				break
			}
			if err.Error() == IfaceNotFound {
				klog.V(3).Infof("Waiting for tunnel interface %s to come up in the pod, retrying",
					KubeTunnelIf)
				continue
			} else {
				break
			}
		}

		if err != nil {
			attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
			return fmt.Errorf("failed to get %s tunnel interface handle due to %v", KubeTunnelIf, err)
		}

		klog.V(2).Infof("Successfully created tunnel interface %s in endpoint %s.",
			KubeTunnelIf, endpointIP)
	}

	// bring the tunnel interface up
	err = netlink.LinkSetUp(tunIf)
	if err != nil {
		attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
		return fmt.Errorf("failed to bring up ipip tunnel interface in endpoint namespace due to %v", err)
	}

	// assign VIP to the KUBE_TUNNEL_IF interface
	err = ln.ipAddrAdd(tunIf, vip, false)
	if err != nil && err.Error() != IfaceHasAddr {
		attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
		return fmt.Errorf("failed to assign vip %s to kube-tunnel-if interface", vip)
	}
	klog.Infof("Successfully assigned VIP: %s in endpoint %s.", vip, endpointIP)

	// disable rp_filter on all interface
	sysctlErr := utils.SetSysctlSingleTemplate(utils.IPv4ConfRPFilterTemplate, "kube-tunnel-if", 0)
	if sysctlErr != nil {
		attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
		return fmt.Errorf("failed to disable rp_filter on kube-tunnel-if in the endpoint container: %s",
			sysctlErr.Error())
	}

	// TODO: it's bad to rely on eth0 here. While this is inside the container's namespace and is determined by the
	// container runtime and so far we've been able to count on this being reliably set to eth0, it is possible that
	// this may shift sometime in the future with a different runtime. It would be better to find a reliable way to
	// determine the interface name from inside the container.
	sysctlErr = utils.SetSysctlSingleTemplate(utils.IPv4ConfRPFilterTemplate, "eth0", 0)
	if sysctlErr != nil {
		attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
		return fmt.Errorf("failed to disable rp_filter on eth0 in the endpoint container: %s", sysctlErr.Error())
	}

	sysctlErr = utils.SetSysctlSingleTemplate(utils.IPv4ConfRPFilterTemplate, "all", 0)
	if sysctlErr != nil {
		attemptNamespaceResetAfterError(hostNetworkNamespaceHandle)
		return fmt.Errorf("failed to disable rp_filter on `all` in the endpoint container: %s", sysctlErr.Error())
	}

	klog.Infof("Successfully disabled rp_filter in endpoint %s.", endpointIP)

	err = netns.Set(hostNetworkNamespaceHandle)
	if err != nil {
		return fmt.Errorf("failed to set hostNetworkNamespace handle due to %v", err)
	}
	activeNetworkNamespaceHandle, err = netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get activeNetworkNamespace handle due to %v", err)
	}
	klog.Infof("Current network namespace after revert namespace to host network namespace: %s",
		activeNetworkNamespaceHandle.String())
	_ = activeNetworkNamespaceHandle.Close()
	return nil
}

// generateUniqueFWMark generates a unique uint32 hash value using the IP address, port, and protocol. This can then
// be used in IPVS, ip rules, and iptables to mark and later identify packets. FWMarks along with ip, port, and protocol
// are then stored in a map on the NSC and can be used later for lookup and as a general translation layer. If after
// maxUniqueFWMarkInc tries, generateUniqueFWMark is not able to find a unique permutation to use, an error is returned.
func (nsc *NetworkServicesController) generateUniqueFWMark(ip, protocol, port string) (uint32, error) {
	// Generate a unit32 hash value using the IP address, port and protocol. This has been moved to an anonymous
	// function since calling this without guarantees of uniqueness is unsafe.
	generateFWMark := func(ip, protocol, port string, increment int) (uint32, error) {
		const maxFwMarkBitSize = 0x3FFF
		var err error
		h := fnv.New32a()
		if increment == 0 {
			_, err = h.Write([]byte(ip + "-" + protocol + "-" + port))
		} else {
			_, err = h.Write([]byte(ip + "-" + protocol + "-" + port + "-" + fmt.Sprintf("%d", increment)))
		}
		if err != nil {
			return 0, err
		}
		return h.Sum32() & maxFwMarkBitSize, err
	}

	const maxUniqueFWMarkInc = 16380
	increment := 0
	serviceKey := fmt.Sprintf("%s-%s-%s", ip, protocol, port)
	for {
		potentialFWMark, err := generateFWMark(ip, protocol, port, increment)
		if err != nil {
			return potentialFWMark, err
		}
		if foundServiceKey, ok := nsc.fwMarkMap[potentialFWMark]; ok {
			if foundServiceKey != serviceKey {
				increment++
				continue
			}
		}
		if increment >= maxUniqueFWMarkInc {
			return 0, fmt.Errorf("could not obtain a unique FWMark for %s:%s:%s after %d tries",
				protocol, ip, port, maxUniqueFWMarkInc)
		}
		nsc.fwMarkMap[potentialFWMark] = serviceKey
		return potentialFWMark, nil
	}
}

// lookupFWMarkByService finds the related FW mark from the internal fwMarkMap kept by the NetworkServiceController
// given the related ip, protocol, and port. If it isn't able to find a matching FW mark, then it returns an error.
func (nsc *NetworkServicesController) lookupFWMarkByService(ip, protocol, port string) (uint32, error) {
	needle := fmt.Sprintf("%s-%s-%s", ip, protocol, port)
	for fwMark, serviceKey := range nsc.fwMarkMap {
		if needle == serviceKey {
			return fwMark, nil
		}
	}
	return 0, fmt.Errorf("no key matching %s:%s:%s was found in fwMarkMap", protocol, ip, port)
}

// lookupServiceByFWMark Lookup service ip, protocol, port by given FW Mark value (reverse of lookupFWMarkByService)
func (nsc *NetworkServicesController) lookupServiceByFWMark(fwMark uint32) (string, string, int, error) {
	serviceKey, ok := nsc.fwMarkMap[fwMark]
	if !ok {
		return "", "", 0, fmt.Errorf("could not find service matching the given FW mark")
	}
	serviceKeySplit := strings.Split(serviceKey, "-")
	if len(serviceKeySplit) != 3 {
		return "", "", 0, fmt.Errorf("service key for found FW mark did not have 3 parts, this shouldn't be possible")
	}
	port, err := strconv.ParseInt(serviceKeySplit[2], 10, 32)
	if err != nil {
		return "", "", 0, fmt.Errorf("port number for service key for found FW mark was not a 32-bit int: %v", err)
	}
	return serviceKeySplit[0], serviceKeySplit[1], int(port), nil
}

// unsortedListsEquivalent compares two lists of endpointsInfo and considers them the same if they contains the same
// contents regardless of order. Returns true if both lists contain the same contents.
func unsortedListsEquivalent(a, b []endpointsInfo) bool {
	if len(a) != len(b) {
		return false
	}

	values := make(map[interface{}]int)
	for _, val := range a {
		values[val] = 1
	}
	for _, val := range b {
		values[val]++
	}

	for _, val := range values {
		if val == 1 {
			return false
		}
	}

	return true
}

// endpointsMapsEquivalent compares two maps of endpointsInfoMap to see if they have the same keys and values. Returns
// true if both maps contain the same keys and values.
func endpointsMapsEquivalent(a, b endpointsInfoMap) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, ok := b[key]
		if !ok {
			return false
		}

		if !unsortedListsEquivalent(valA, valB) {
			return false
		}
	}

	return true
}
