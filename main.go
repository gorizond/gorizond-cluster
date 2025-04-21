package main

import (
	"flag"
	controllers "github.com/gorizond/gorizond-cluster/controllers"
	"github.com/gorizond/gorizond-cluster/pkg"
	controllersNetwork "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/networking.k8s.io"
	controllersGorizond "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	controllersTraefik "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/traefik.io"
	"github.com/joho/godotenv"
	"github.com/rancher/lasso/pkg/log"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"

	"github.com/rancher/wrangler/v3/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/rancher/wrangler/v3/pkg/start"
	"k8s.io/client-go/rest"
	"os"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Infof(".env file not found")
	}

	var kubeconfig_file string
	flag.StringVar(&kubeconfig_file, "kubeconfig", "", "Path to kubeconfig")
	flag.Parse()

	dbHeadScale, err := pkg.NewDatabaseManager(os.Getenv("DB_DSN_HEADSCALE"))
	if err != nil {
		panic(err)
	}

	dbKubernetes, err := pkg.NewDatabaseManager(os.Getenv("DB_DSN_KUBERNETES"))
	if err != nil {
		panic(err)
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Infof("Error getting kubernetes config: %v", err)
		config, err = kubeconfig.GetNonInteractiveClientConfig(kubeconfig_file).ClientConfig()
		if err != nil {
			panic(err)
		}
		log.Infof("Using kubeconfig file")
	}

	mgmtGorizond, err := controllersGorizond.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtProvision, err := controllersProvision.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtCore, err := core.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtApps, err := apps.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtNetwork, err := controllersNetwork.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtTraefik, err := controllersTraefik.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	mgmtManagement, err := controllersManagement.NewFactoryFromConfig(config)
	if err != nil {
		panic(err)
	}
	ctx := signals.SetupSignalContext()

	controllers.InitClusterController(ctx, mgmtGorizond, mgmtProvision, mgmtCore, mgmtApps, mgmtNetwork, mgmtTraefik, dbHeadScale, dbKubernetes)
	controllers.InitRegistrationToken(ctx, mgmtManagement, mgmtCore, mgmtApps)

	if err := start.All(ctx, 10, mgmtGorizond, mgmtProvision, mgmtApps, mgmtManagement); err != nil {
		panic(err)
	}

	go dbHeadScale.StartCronJob()
	go dbKubernetes.StartCronJob()

	<-ctx.Done()
}
