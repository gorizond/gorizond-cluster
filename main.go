package main

import (
	"flag"
	"os"

	controllers "github.com/gorizond/gorizond-cluster/controllers"
	controllersGorizond "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	"github.com/joho/godotenv"
	"github.com/rancher/lasso/pkg/log"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"

	"github.com/rancher/wrangler/v3/pkg/apply"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/rancher/wrangler/v3/pkg/start"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func main() {

	ctx := signals.SetupSignalContext()

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
	var starters []start.Starter
	var mgmtManagement *controllersManagement.Factory
	var mgmtGorizond *controllersGorizond.Factory
	if os.Getenv("ENABLE_CONTROLLER_GORIZOND") == "true" {
		mgmtGorizond, err = controllersGorizond.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}
		mgmtProvision, err := controllersProvision.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}
		mgmtManagement, err = controllersManagement.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}
		mgmtCore, err := core.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}

		dynamicClient, err := dynamic.NewForConfig(configRancher)
		if err != nil {
			panic(err)
		}

		controllers.InitClusterController(ctx, mgmtGorizond, mgmtManagement, mgmtProvision, mgmtCore, dynamicClient)
		starters = append(starters, mgmtGorizond, mgmtProvision)
	}
	if os.Getenv("ENABLE_CONTROLLER_BILLING") == "true" {
		mgmtManagement, err = controllersManagement.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}
		mgmtGorizond, err = controllersGorizond.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}
		mgmtCore, err := core.NewFactoryFromConfig(configRancher)
		if err != nil {
			panic(err)
		}

		apply, err := apply.NewForConfig(configRancher)
		if err != nil {
			panic(err)
		}
		controllers.InitBillingClusterController(ctx, mgmtManagement, mgmtGorizond, mgmtCore)
		controllers.InitBillingEventController(ctx, mgmtGorizond.Provisioning().V1().Billing(), mgmtGorizond.Provisioning().V1().BillingEvent(), apply, 4)
		controllers.InitPeriodicBillingController(ctx, mgmtGorizond.Provisioning().V1().Cluster(), mgmtGorizond.Provisioning().V1().BillingEvent())
		starters = append(starters, mgmtManagement, mgmtGorizond, mgmtCore)
	}
	// start controllers
	if err := start.All(ctx, 50, starters...); err != nil {
		panic(err)
	}

	<-ctx.Done()
}
