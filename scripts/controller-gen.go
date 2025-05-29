package main

import (
	gorizondV1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
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
		},
	})
}
