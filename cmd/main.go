package main

import (
	"flag"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/controller"
	"github.com/mborodin/uyuni-operator/internal/pool"
	"github.com/mborodin/uyuni-operator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(uyuniv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var operatorNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for high availability.")
	flag.StringVar(&operatorNamespace, "namespace", "uyuni-operator-system", "Namespace the operator runs in; credential Secrets are read from here.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "uyuni-operator.uyuni-project.org",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	clientPool := pool.New(mgr.GetClient(), operatorNamespace)

	if err := (&controller.OrganizationReconciler{
		Client:     mgr.GetClient(),
		Clients:    clientPool,
		OperatorNS: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Organization")
		os.Exit(1)
	}

	if err := (&controller.UyuniProviderReconciler{
		Client:     mgr.GetClient(),
		Pool:       clientPool,
		OperatorNS: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "UyuniProvider")
		os.Exit(1)
	}

	if err := (&controller.SystemGroupReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SystemGroup")
		os.Exit(1)
	}

	if err := (&controller.SystemReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
		Now:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "System")
		os.Exit(1)
	}

	if err := (&controller.ActivationKeyReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ActivationKey")
		os.Exit(1)
	}

	if err := (&controller.RepositoryReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Repository")
		os.Exit(1)
	}

	if err := (&controller.SoftwareChannelReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SoftwareChannel")
		os.Exit(1)
	}

	if err := (&controller.ContentProjectReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
		Now:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ContentProject")
		os.Exit(1)
	}

	if err := (&controller.ContentProjectPromotionReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
		Now:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ContentProjectPromotion")
		os.Exit(1)
	}

	if err := (&controller.TaskReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
		Now:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Task")
		os.Exit(1)
	}

	if err := (&controller.ConfigurationChannelReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ConfigurationChannel")
		os.Exit(1)
	}

	if err := (&controller.ClmEnvironmentReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClmEnvironment")
		os.Exit(1)
	}

	if err := (&controller.AutoinstallDistributionReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AutoinstallDistribution")
		os.Exit(1)
	}

	if err := (&controller.CustomInfoKeyReconciler{
		Client:  mgr.GetClient(),
		Clients: clientPool,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CustomInfoKey")
		os.Exit(1)
	}

	if err := (&webhook.OrganizationValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "OrganizationValidator")
		os.Exit(1)
	}

	if err := (&webhook.UyuniProviderValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "UyuniProviderValidator")
		os.Exit(1)
	}

	if err := (&webhook.ActivationKeyValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ActivationKeyValidator")
		os.Exit(1)
	}

	if err := (&webhook.SystemDefaulter{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SystemDefaulter")
		os.Exit(1)
	}

	if err := (&webhook.SystemValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SystemValidator")
		os.Exit(1)
	}

	if err := (&webhook.ContentProjectValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ContentProjectValidator")
		os.Exit(1)
	}

	if err := (&webhook.ContentProjectPromotionValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ContentProjectPromotionValidator")
		os.Exit(1)
	}

	if err := (&webhook.TaskValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "TaskValidator")
		os.Exit(1)
	}

	if err := (&webhook.ConfigurationChannelValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ConfigurationChannelValidator")
		os.Exit(1)
	}

	if err := (&webhook.SystemGroupValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SystemGroupValidator")
		os.Exit(1)
	}

	if err := (&webhook.ClmEnvironmentValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "ClmEnvironmentValidator")
		os.Exit(1)
	}

	if err := (&webhook.AutoinstallDistributionValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AutoinstallDistributionValidator")
		os.Exit(1)
	}

	if err := (&webhook.CustomInfoKeyValidator{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CustomInfoKeyValidator")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
