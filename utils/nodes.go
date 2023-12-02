package utils

import (
	v1 "k8s.io/api/core/v1"
	"net"
)

func GetHostIP(node v1.Node) string {
	hostIP := node.ObjectMeta.Name
	if net.ParseIP(hostIP) != nil {
		return hostIP
	} else {
		// get node ip from node status
		for _, address := range node.Status.Addresses {
			switch addressType := address.Type; addressType {
			case v1.NodeInternalIP:
				hostIP = address.Address
				break
			}
		}
	}
	return hostIP
}
