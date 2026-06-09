/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/go-logr/logr"

	omsv1alpha1 "image-patch-operator/api/v1alpha1"
	"image-patch-operator/internal/controller"
	"image-patch-operator/internal/metrics"
	"image-patch-operator/internal/registry"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(omsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// dedupRegistryClient constructs the registry client used for the
// dedup short-circuit. Fail-open at startup: if dedup is disabled, or
// the config path is missing / unreadable, we log and return nil --
// the controller still runs (and Kaniko-side dedup writes still
// happen via multi-destination push). Only the short-circuit (HEAD +
// retag) is gated on a working client.
//
// REGISTRY_AUTH_CONFIG_PATH defaults to /registry/.docker/config.json,
// matching the chart's mount path for the image-registry-secret.
func dedupRegistryClient(dedupEnabled bool, log logr.Logger) *registry.Client {
	if !dedupEnabled {
		return nil
	}
	path := os.Getenv("REGISTRY_AUTH_CONFIG_PATH")
	if path == "" {
		path = "/registry/.docker/config.json"
	}
	c, err := registry.NewFromDockerConfig(path)
	if err != nil {
		log.Info("dedup short-circuit disabled: docker config not loadable; Kaniko-side dedup write is unaffected", "path", path, "err", err.Error())
		return nil
	}
	log.Info("dedup short-circuit enabled", "configPath", path)
	return c
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Scope the Owns(...) informers to the build namespace. SetupWithManager
	// declares Owns on Job / ConfigMap / Secret, which unconditionally start
	// informers at manager boot -- with the new RBAC (Role in the build ns
	// instead of cluster-wide ClusterRole), a cluster-wide list/watch would
	// 403 and cache sync would never complete. ImagePatch is intentionally
	// not in this map so the CR informer remains cluster-wide (users create
	// CRs in their own namespaces). pods/log is fetched via the typed
	// kubernetes clientset and does not go through the cache.
	buildNs := os.Getenv("BUILD_NAMESPACE")
	if buildNs == "" {
		buildNs = "image-patch-system"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "da16e050.oms.ogpu.cloud",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{buildNs: {}}},
				&corev1.Secret{}:    {Namespaces: map[string]cache.Config{buildNs: {}}},
				&batchv1.Job{}:      {Namespaces: map[string]cache.Config{buildNs: {}}},
			},
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	kanikoImage := os.Getenv("KANIKO_IMAGE_NAME")
	if kanikoImage == "" {
		kanikoImage = "gcr.io/kaniko-project/executor:v1.23.2"
	}

	dedupEnabled := os.Getenv("DEDUP_ENABLED") != "false"
	registryClient := dedupRegistryClient(dedupEnabled, setupLog)

	// Typed clientset for subresource access (pods/log) -- the
	// controller-runtime Client does not expose those. classifyBuildFailure
	// uses it to tail the Kaniko build Pod when a Job fails; without this
	// wiring the classifier silently returns ControllerInternalError on
	// every failure, mis-blaming us for what is often bad-input
	// (BaseImageNotFound, AuthorizationNeeded, etc.). Constructed once
	// here so the reconciler doesn't redial per call.
	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to construct kubernetes clientset")
		os.Exit(1)
	}

	if err := (&controller.ImagePatchReconciler{
		Client:                   mgr.GetClient(),
		Kubernetes:               kubeClient,
		Scheme:                   mgr.GetScheme(),
		DefaultRegistry:          os.Getenv("DEFAULT_IMAGE_REGISTRY"),
		KanikoImage:              kanikoImage,
		KanikoBuildCacheRepo:     os.Getenv("KANIKO_BUILD_CACHE_REPO"),
		BuildNamespace:           os.Getenv("BUILD_NAMESPACE"),
		DefaultBuildOptions:      controller.BuildOptionsFromEnv(),
		KanikoResources:          controller.KanikoResourcesFromEnv(setupLog),
		KanikoRegistryMirrors:    controller.RegistryMirrorsFromEnv(),
		DedupEnabled:             dedupEnabled,
		Registry:                 registryClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImagePatch")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Periodic CR-phase gauges live outside the reconcile loop because
	// per-CR reconciles only refresh the gauge for the CR being reconciled
	// -- a fully idle cluster would never re-publish them.
	if err := mgr.Add(&metrics.PhaseAggregator{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to register phase aggregator")
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
