package main

import (
	"flag"
	controllers "github.com/gorizond/gorizond-cluster/controllers"
	controllersGorizond "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	"github.com/joho/godotenv"
	"github.com/rancher/lasso/pkg/log"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"
	
	controllersFleet "github.com/rancher/rancher/pkg/generated/controllers/fleet.cattle.io"

	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/rancher/wrangler/v3/pkg/start"
	"k8s.io/client-go/rest"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Infof(".env file not found")
	}

	var kubeconfig_file_rancher string
	flag.StringVar(&kubeconfig_file_rancher, "kubeconfig-rancher", "", "Path to kubeconfig for rancher cluster")
	flag.Parse()

	var configRancher *rest.Config
	if kubeconfig_file_rancher != "" {
		configRancher, err = kubeconfig.GetNonInteractiveClientConfig(kubeconfig_file_rancher).ClientConfig()
		if err != nil {
			panic(err)
		}
		log.Infof("Using kubeconfig file for rancher")
	} else {
		configRancher, err = rest.InClusterConfig()
		if err != nil {
			log.Infof("Error getting kubernetes config for rancher: %v", err)
			panic(err)
		}
	}

	mgmtGorizond, err := controllersGorizond.NewFactoryFromConfig(configRancher)
	if err != nil {
		panic(err)
	}
	mgmtProvision, err := controllersProvision.NewFactoryFromConfig(configRancher)
	if err != nil {
		panic(err)
	}
	mgmtManagement, err := controllersManagement.NewFactoryFromConfig(configRancher)
	if err != nil {
		panic(err)
	}
	mgmtFleet, err := controllersFleet.NewFactoryFromConfig(configRancher)
	if err != nil {
		panic(err)
	}

	mgmtCore, err := core.NewFactoryFromConfig(configRancher)
	if err != nil {
		panic(err)
	}

	ctx := signals.SetupSignalContext()

	controllers.InitClusterController(ctx, mgmtGorizond, mgmtManagement, mgmtProvision, mgmtCore, mgmtFleet)

	if err := start.All(ctx, 10, mgmtGorizond, mgmtProvision); err != nil {
		panic(err)
	}


	<-ctx.Done()
}
