/*
Copyright 2022 k0s authors

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
package controller

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/avast/retry-go"
	"github.com/bombsimon/logrusr/v2"
	"github.com/k0sproject/k0s/internal/pkg/templatewriter"
	"github.com/k0sproject/k0s/pkg/apis/helm.k0sproject.io/v1beta1"
	k0sAPI "github.com/k0sproject/k0s/pkg/apis/k0s.k0sproject.io/v1beta1"
	"github.com/k0sproject/k0s/pkg/component"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/helm"
	kubeutil "github.com/k0sproject/k0s/pkg/kubernetes"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Helm watch for Chart crd
type ExtensionsController struct {
	saver         manifestsSaver
	L             *logrus.Entry
	helm          *helm.Commands
	kubeConfig    string
	leaderElector LeaderElector
}

var _ component.Component = &ExtensionsController{}
var _ component.ReconcilerComponent = &ExtensionsController{}

// NewExtensionsController builds new HelmAddons
func NewExtensionsController(s manifestsSaver, k0sVars constant.CfgVars, kubeClientFactory kubeutil.ClientFactoryInterface, leaderElector LeaderElector) *ExtensionsController {
	return &ExtensionsController{
		saver:         s,
		L:             logrus.WithFields(logrus.Fields{"component": "extensions_controller"}),
		helm:          helm.NewCommands(k0sVars),
		kubeConfig:    k0sVars.AdminKubeConfigPath,
		leaderElector: leaderElector,
	}
}

const (
	namespaceToWatch = "kube-system"
)

// Run runs the extensions controller
func (ec *ExtensionsController) Reconcile(ctx context.Context, clusterConfig *k0sAPI.ClusterConfig) error {
	ec.L.Info("Extensions reconcilation started")
	defer ec.L.Info("Extensions reconcilation finished")

	helmSettings := clusterConfig.Spec.Extensions.Helm
	switch clusterConfig.Spec.Extensions.Storage.Type {
	case k0sAPI.OpenEBSLocal:
		helmSettings = addOpenEBSHelmExtension(helmSettings)
	default:
	}

	if err := ec.reconcileHelmExtensions(helmSettings); err != nil {
		return fmt.Errorf("can't reconcile helm based extensions: %v", err)
	}

	return nil
}

func addOpenEBSHelmExtension(helmSpec *k0sAPI.HelmExtensions) *k0sAPI.HelmExtensions {
	if helmSpec == nil {
		helmSpec = &k0sAPI.HelmExtensions{
			Repositories: k0sAPI.RepositoriesSettings{},
			Charts:       k0sAPI.ChartsSettings{},
		}
	}
	helmSpec.Repositories = append(helmSpec.Repositories, k0sAPI.Repository{
		Name: "openebs-internal",
		URL:  constant.OpenEBSRepository,
	})
	helmSpec.Charts = append(helmSpec.Charts, k0sAPI.Chart{
		Name:      "openebs",
		ChartName: "openebs-internal/openebs",
		TargetNS:  "openebs",
		Version:   constant.OpenEBSVersion,
	})
	return helmSpec
}

// reconcileHelmExtensions creates instance of Chart CR for each chart of the config file
// it also reconciles repositories settings
// the actual helm install/update/delete management is done by ChartReconciler structure
func (ec *ExtensionsController) reconcileHelmExtensions(helmSpec *k0sAPI.HelmExtensions) error {
	if helmSpec == nil {
		return nil
	}

	for _, repo := range helmSpec.Repositories {
		if err := ec.addRepo(repo); err != nil {
			return fmt.Errorf("can't init repository `%s`: %v", repo.URL, err)
		}
	}

	for _, chart := range helmSpec.Charts {
		tw := templatewriter.TemplateWriter{
			Name:     "addon_crd_manifest",
			Template: chartCrdTemplate,
			Data: struct {
				k0sAPI.Chart
				Finalizer string
			}{
				Chart:     chart,
				Finalizer: finalizerName,
			},
		}
		buf := bytes.NewBuffer([]byte{})
		if err := tw.WriteToBuffer(buf); err != nil {
			ec.L.WithError(err).Errorf("can't create chart CR instance `%s`: %v", chart.ChartName, err)
			return fmt.Errorf("can't create chart CR instance `%s`: %v", chart.ChartName, err)
		}
		if err := ec.saver.Save("addon_crd_manifest_"+chart.Name+".yaml", buf.Bytes()); err != nil {
			return fmt.Errorf("can't save addon CRD manifest: %v", err)
		}
	}
	return nil
}

type ChartReconciler struct {
	client.Client
	helm          *helm.Commands
	leaderElector LeaderElector
	L             *logrus.Entry
}

func (cr *ChartReconciler) InjectClient(c client.Client) error {
	cr.Client = c
	return nil
}

func (cr *ChartReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if !cr.leaderElector.IsLeader() {
		return reconcile.Result{}, nil
	}
	cr.L.Tracef("Got helm chart reconcilation request: %s", req)
	defer cr.L.Tracef("Finished processing helm chart reconcilation request: %s", req)

	var chartInstance v1beta1.Chart

	if err := cr.Client.Get(ctx, req.NamespacedName, &chartInstance); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !chartInstance.ObjectMeta.DeletionTimestamp.IsZero() {
		cr.L.Tracef("Uninstall reconcilation request: %s", req)
		// uninstall chart
		if err := cr.uninstall(ctx, chartInstance); err != nil {
			return reconcile.Result{}, fmt.Errorf("can't uninstall chart: %w", err)
		}
		controllerutil.RemoveFinalizer(&chartInstance, finalizerName)
		if err := cr.Client.Update(ctx, &chartInstance); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
	cr.L.Tracef("Install or update reconcilation request: %s", req)
	if err := cr.updateOrInstallChart(ctx, chartInstance); err != nil {
		return reconcile.Result{Requeue: true}, fmt.Errorf("can't update or install chart: %w", err)
	}
	return reconcile.Result{}, nil
}
func (cr *ChartReconciler) uninstall(ctx context.Context, chart v1beta1.Chart) error {
	if err := cr.helm.UninstallRelease(chart.Status.ReleaseName, chart.Status.Namespace); err != nil {
		return fmt.Errorf("can't uninstall release `%s/%s`: %v", chart.Status.Namespace, chart.Status.ReleaseName, err)
	}
	return nil
}

func (cr *ChartReconciler) updateOrInstallChart(ctx context.Context, chart v1beta1.Chart) error {
	var err error
	var chartRelease *release.Release
	if chart.Status.ReleaseName == "" {
		// new chartRelease
		chartRelease, err = cr.helm.InstallChart(chart.Spec.ChartName,
			chart.Spec.Version,
			chart.Spec.Namespace,
			chart.Spec.YamlValues())
		if err != nil {
			return fmt.Errorf("can't reconcile installation for `%s`: %v", chart.GetName(), err)
		}
	} else {
		// update
		chartRelease, err = cr.helm.UpgradeChart(chart.Spec.ChartName,
			chart.Status.Version,
			chart.Status.ReleaseName,
			chart.Status.Namespace,
			chart.Spec.YamlValues(),
		)
		if err != nil {
			return fmt.Errorf("can't reconcile upgrade for `%s`: %v", chart.GetName(), err)
		}
	}

	chart.Status.ReleaseName = chartRelease.Name
	chart.Status.Version = chartRelease.Chart.Metadata.Version
	chart.Status.AppVersion = chartRelease.Chart.AppVersion()
	chart.Status.Updated = time.Now().String()
	chart.Status.Revision = int64(chartRelease.Version)
	chart.Status.Namespace = chartRelease.Namespace
	chart.Status.Error = ""
	err = cr.Client.Status().Update(ctx, &chart)
	if err != nil {
		return fmt.Errorf("can't update status for `%s`: %v", chart.GetName(), err)
	}
	return nil
}

func (ec *ExtensionsController) addRepo(repo k0sAPI.Repository) error {
	return ec.helm.AddRepository(repo)
}

const chartCrdTemplate = `
apiVersion: helm.k0sproject.io/v1beta1
kind: Chart
metadata:
  name: k0s-addon-chart-{{ .Name }}
  namespace: "kube-system"
  finalizers:
    - {{ .Finalizer }}
spec:
  chartName: {{ .ChartName }}
  values: |
{{ .Values | nindent 4 }}
  version: {{ .Version }}
  namespace: {{ .TargetNS }}
`

const finalizerName = "helm.k0sproject.io/uninstall-helm-release"

// Init
func (ec *ExtensionsController) Init(_ context.Context) error {
	return nil
}

// Run
func (ec *ExtensionsController) Run(ctx context.Context) error {
	config, err := clientcmd.BuildConfigFromFlags("", ec.kubeConfig)
	if err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}

	mgr, err := manager.New(config, manager.Options{
		MetricsBindAddress: "0",
		Logger:             logrusr.New(ec.L),
	})
	if err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}
	if err := retry.Do(func() error {
		_, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{
			Group: v1beta1.GroupVersion.Group,
			Kind:  "Chart",
		})
		if err != nil {
			ec.L.Warn("Extensions CRD is not yet ready, waiting before starting ExtensionsController")
			return err
		}
		ec.L.Info("Extensions CRD is ready, going nuts")
		return nil
	}, retry.Context(ctx)); err != nil {
		return fmt.Errorf("can't start ExtensionsReconciler, helm CRD is not registred, check CRD registration reconciler: %w", err)
	}
	// examples say to not use GetScheme in production, but it is unclear at the moment
	// which scheme should be in use
	if err := v1beta1.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("can't register Chart crd: %w", err)
	}

	if err := builder.
		ControllerManagedBy(mgr).
		For(&v1beta1.Chart{},
			builder.WithPredicates(predicate.And(
				predicate.GenerationChangedPredicate{},
				predicate.NewPredicateFuncs(func(object client.Object) bool {
					return object.GetNamespace() == namespaceToWatch
				}),
			),
			),
		).
		Complete(&ChartReconciler{
			leaderElector: ec.leaderElector, // TODO: drop in favor of controller-runtime lease manager?
			helm:          ec.helm,
			L:             ec.L.WithField("extensions_type", "helm"),
		}); err != nil {
		return fmt.Errorf("can't build controller-runtime controller for helm extensions: %w", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			ec.L.Tracef("controller-runtime working loop finished: %s", err)
		}
	}()

	return nil
}

// Stop
func (ec *ExtensionsController) Stop() error {
	return nil
}

// Healthy
func (ec *ExtensionsController) Healthy() error {
	return nil
}
