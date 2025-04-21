package main

import (
	networkingV1 "github.com/gorizond/gorizond-cluster/pkg/apis/networking.k8s.io/v1"
	gorizondV1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	traefikV1 "github.com/gorizond/gorizond-cluster/pkg/apis/traefik.io/v1alpha1"
	controllergen "github.com/rancher/wrangler/v3/pkg/controller-gen"
	"github.com/rancher/wrangler/v3/pkg/controller-gen/args"
)

func main() {
	controllergen.Run(args.Options{
		OutputPackage: "github.com/gorizond/gorizond-cluster/pkg/generated",
		Boilerplate:   "scripts/boilerplate.go.txt",
		Groups: map[string]args.Group{
			"provisioning.gorizond.io": {
				PackageName: "provisioning.gorizond.io",
				Types: []interface{}{
					gorizondV1.Cluster{},
				},
				GenerateTypes: true,
			},
			"networking.k8s.io": {
				PackageName: "networking.k8s.io",
				Types: []interface{}{
					networkingV1.Ingress{},
				},
				GenerateTypes: true,
			},
			"traefik.io": {
				PackageName: "traefik.io",
				Types: []interface{}{
					traefikV1.IngressRouteTCP{},
				},
				GenerateTypes: true,
			},
		},
	})
}
