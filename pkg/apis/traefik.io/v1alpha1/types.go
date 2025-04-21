package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IngressRouteTCP is the CRD implementation of a Traefik TCP Router
// +kubebuilder:resource:path=ingressroutetcp,scope=Namespaced
// +kubebuilder:storageversion
type IngressRouteTCP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IngressRouteTCPSpec `json:"spec"`
}

// IngressRouteTCPSpec defines the desired state of IngressRouteTCP
type IngressRouteTCPSpec struct {
	EntryPoints []string   `json:"entryPoints,omitempty"`
	Routes      []RouteTCP `json:"routes"`
	TLS         *TLSTCP    `json:"tls,omitempty"`
}

// RouteTCP contains the set of routes
type RouteTCP struct {
	Match       string          `json:"match"`
	Middlewares []MiddlewareRef `json:"middlewares,omitempty"`
	Priority    int             `json:"priority,omitempty"`
	Services    []ServiceTCP    `json:"services"`
}

// MiddlewareRef is a reference to a Middleware resource
type MiddlewareRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// ServiceTCP defines an upstream TCP service to proxy traffic to
type ServiceTCP struct {
	Name             string         `json:"name"`
	Namespace        string         `json:"namespace,omitempty"`
	NativeLB         bool           `json:"nativeLB,omitempty"`
	Port             int            `json:"port"`
	ProxyProtocol    *ProxyProtocol `json:"proxyProtocol,omitempty"`
	TerminationDelay *int           `json:"terminationDelay,omitempty"`
	Weight           int            `json:"weight,omitempty"`
}

// ProxyProtocol defines the PROXY protocol configuration
type ProxyProtocol struct {
	Version int `json:"version,omitempty"`
}

// TLSTCP contains the TLS certificates configuration
type TLSTCP struct {
	CertResolver string            `json:"certResolver,omitempty"`
	Domains      []Domain          `json:"domains,omitempty"`
	Options      *OptionsReference `json:"options,omitempty"`
	Passthrough  bool              `json:"passthrough,omitempty"`
	SecretName   string            `json:"secretName,omitempty"`
	Store        *StoreReference   `json:"store,omitempty"`
}

// Domain holds a domain name with SANs
type Domain struct {
	Main string   `json:"main,omitempty"`
	Sans []string `json:"sans,omitempty"`
}

// OptionsReference is a reference to a TLSOption resource
type OptionsReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// StoreReference is a reference to a TLSStore resource
type StoreReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}
