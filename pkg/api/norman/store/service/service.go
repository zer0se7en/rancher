package service

import (
	"fmt"
	"net"
	"strconv"

	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	v3 "github.com/rancher/rancher/pkg/client/generated/project/v3"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func New(store types.Store) types.Store {
	return &Store{
		store,
	}
}

type Store struct {
	types.Store
}

func (p *Store) Create(apiContext *types.APIContext, schema *types.Schema, data map[string]interface{}) (map[string]interface{}, error) {
	if schema.ID == "dnsRecord" {
		if convert.IsAPIObjectEmpty(data["hostname"]) {
			data["kind"] = "ClusterIP"
			data["clusterIp"] = nil
		} else {
			data["kind"] = "ExternalName"
			data["clusterIp"] = ""
		}
	}
	if schema.ID == "service" {
		logrus.Tracef("Service: Create: data.kind [%v], data.clusterIp [%v]", data["kind"], data["clusterIp"])
		if data["kind"] == "ClusterIP" && data["clusterIp"] == nil {
			if data["ipFamilyPolicy"] == nil {
				logrus.Debugf("Setting ipFamilyPolicy to SingleStack for service name [%s] service kind [%s]", data["name"], data["kind"])
				data["ipFamilyPolicy"] = "SingleStack"
			}
		}
	}
	formatData(schema, data)
	err := p.validateNonSpecialIP(schema, data)
	if err != nil {
		return nil, err
	}
	return p.Store.Create(apiContext, schema, data)
}

func formatData(schema *types.Schema, data map[string]interface{}) {
	var ports []interface{}
	if schema.ID == "service" {
		ports = convert.ToInterfaceSlice(data["ports"])
	}
	// append default port as sky dns won't work w/o at least one port being set
	if len(ports) == 0 {
		servicePort := v3.ServicePort{
			Port:       42,
			TargetPort: intstr.Parse(strconv.FormatInt(42, 10)),
			Protocol:   "TCP",
			Name:       "default",
		}
		m, err := convert.EncodeToMap(servicePort)
		if err != nil {
			logrus.Warnf("Failed to transform service port to map: %v", err)
			return
		}
		ports = append(ports, m)
	}
	data["ports"] = ports
}

func (p *Store) validateNonSpecialIP(schema *types.Schema, data map[string]interface{}) error {
	if schema.ID == "dnsRecord" {
		ips := data["ipAddresses"]
		if ips != nil {
			for _, ip := range ips.([]interface{}) {
				IP := net.ParseIP(ip.(string))
				if IP == nil {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("%s must be a valid IP address", IP))
				}
				if IP.IsUnspecified() {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("%s may not be unspecified (0.0.0.0)", IP))
				}
				if IP.IsLoopback() {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("%s may not be in the loopback range (127.0.0.0/8)", IP))
				}
				if IP.IsLinkLocalUnicast() {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("%s may not be in the link-local range (169.254.0.0/16)", IP))
				}
				if IP.IsLinkLocalMulticast() {
					return httperror.NewAPIError(httperror.InvalidOption, fmt.Sprintf("%s may not be in the link-local multicast range (224.0.0.0/24)", IP))
				}
			}
		}
	}
	return nil
}
