package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Ingress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IngressSpec   `json:"spec,omitempty"`
	Status            IngressStatus `json:"status,omitempty"`
}

type IngressSpec struct {
	DefaultBackend   *IngressBackend `json:"defaultBackend,omitempty"`
	IngressClassName *string         `json:"ingressClassName,omitempty"`
	Rules            []IngressRule   `json:"rules,omitempty"`
	TLS              []IngressTLS    `json:"tls,omitempty"`
}

type IngressRule struct {
	Host string                `json:"host,omitempty"`
	HTTP *HTTPIngressRuleValue `json:"http,omitempty"`
}

type HTTPIngressRuleValue struct {
	Paths []HTTPIngressPath `json:"paths"`
}

type HTTPIngressPath struct {
	Path     string         `json:"path,omitempty"`
	PathType *string        `json:"pathType"`
	Backend  IngressBackend `json:"backend"`
}

type IngressBackend struct {
	Service  *IngressServiceBackend     `json:"service,omitempty"`
	Resource *TypedLocalObjectReference `json:"resource,omitempty"`
}

type IngressServiceBackend struct {
	Name string             `json:"name"`
	Port ServiceBackendPort `json:"port"`
}

type ServiceBackendPort struct {
	Name   string `json:"name,omitempty"`
	Number int32  `json:"number,omitempty"`
}

type TypedLocalObjectReference struct {
	APIGroup *string `json:"apiGroup,omitempty"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
}

type IngressTLS struct {
	Hosts      []string `json:"hosts,omitempty"`
	SecretName string   `json:"secretName,omitempty"`
}

type IngressStatus struct {
	LoadBalancer IngressLoadBalancerStatus `json:"loadBalancer,omitempty"`
}

type IngressLoadBalancerStatus struct {
	Ingress []IngressLoadBalancerIngress `json:"ingress,omitempty"`
}

type IngressLoadBalancerIngress struct {
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}
