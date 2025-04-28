package main

import (
	"flag"
	controllers "github.com/gorizond/gorizond-cluster/controllers"
	"github.com/gorizond/gorizond-cluster/pkg"
	controllersNetwork "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/networking.k8s.io"
	controllersGorizond "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	"github.com/joho/godotenv"
	"github.com/rancher/lasso/pkg/log"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"

	"github.com/rancher/wrangler/v3/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/batch"
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

	var kubeconfig_file_data_cluster string
	var kubeconfig_file_rancher string
	flag.StringVar(&kubeconfig_file_data_cluster, "kubeconfig-data", "", "Path to kubeconfig for k3s, headscale")
	flag.StringVar(&kubeconfig_file_rancher, "kubeconfig-rancher", "", "Path to kubeconfig for rancher cluster")
	flag.Parse()

	dbHeadScale, err := pkg.NewDatabaseManager(os.Getenv("DB_DSN_HEADSCALE"))
	if err != nil {
		panic(err)
	}

	dbKubernetes, err := pkg.NewDatabaseManager(os.Getenv("DB_DSN_KUBERNETES"))
	if err != nil {
		panic(err)
	}

	var configDataCluster *rest.Config
	if kubeconfig_file_data_cluster != "" {
		configDataCluster, err = kubeconfig.GetNonInteractiveClientConfig(kubeconfig_file_data_cluster).ClientConfig()
		if err != nil {
			panic(err)
		}
		log.Infof("Using kubeconfig file for data cluster")
	} else {
		configDataCluster, err = rest.InClusterConfig()
		if err != nil {
			log.Infof("Error getting kubernetes config for data cluster: %v", err)
			panic(err)
		}
	}

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

	mgmtCore, err := core.NewFactoryFromConfig(configDataCluster)
	if err != nil {
		panic(err)
	}
		
	mgmtBatch, err := batch.NewFactoryFromConfig(configDataCluster)
	if err != nil {
		panic(err)
	}
	
	mgmtApps, err := apps.NewFactoryFromConfig(configDataCluster)
	if err != nil {
		panic(err)
	}
	mgmtNetwork, err := controllersNetwork.NewFactoryFromConfig(configDataCluster)
	if err != nil {
		panic(err)
	}
	ctx := signals.SetupSignalContext()

	controllers.InitClusterController(ctx, mgmtGorizond, mgmtProvision, mgmtCore, mgmtApps, mgmtNetwork, dbHeadScale, dbKubernetes)
	controllers.InitRegistrationToken(ctx, mgmtManagement, mgmtCore, mgmtApps)
	controllers.InitReconnectCluster(ctx, mgmtProvision, mgmtBatch)

	if err := start.All(ctx, 10, mgmtGorizond, mgmtProvision, mgmtApps, mgmtManagement); err != nil {
		panic(err)
	}

	go dbHeadScale.StartCronJob()
	go dbKubernetes.StartCronJob()

	<-ctx.Done()
}
